//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const processAdmissionNamespace int32 = 1129862210
const processAdmissionIOTimeout = time.Second
const processAdmissionPrefix = "cxthub/bounded-documents-v1/"

type postgresProcessAdmission struct {
	pool        *pgxpool.Pool
	control     *pgxpool.Conn
	controlPID  uint32
	controlUse  chan struct{}
	roleKey     int32
	bulkKey     int32
	bulkName    string
	fingerprint string
	active      chan struct{}
	bulk        chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	ready       bool
	readyCtx    context.Context
	readyCancel context.CancelFunc
	readyErr    error
	err         error
	closing     bool
	bulkPID     uint32
	bulkEpoch   uint64
	permits     sync.WaitGroup
	queries     sync.WaitGroup
	heartbeat   chan struct{}
	closed      chan struct{}
	closeErr    error
}

type postgresProcessPermit struct {
	controller  *postgresProcessAdmission
	ctx         context.Context
	cancel      context.CancelFunc
	operation   chan struct{}
	bulkConn    *pgxpool.Conn
	bulkWanted  bool
	active      bool
	parked      bool
	finished    bool
	releaseOnce sync.Once
	releaseErr  error
}

var _ outbound.ProcessAdmissionStore = (*PostgresStore)(nil)
var _ outbound.ProcessAdmission = (*postgresProcessAdmission)(nil)
var _ outbound.ProcessAdmissionPermit = (*postgresProcessPermit)(nil)

func processAdmissionRole(role outbound.ProcessRole) (int32, int32, int, bool) {
	switch role {
	case outbound.ProcessAPI:
		return 10, 20, 2, true
	case outbound.ProcessMCP:
		return 11, 20, 1, true
	case outbound.ProcessWorker:
		return 12, 21, 1, true
	default:
		return 0, 0, 0, false
	}
}

func (s *PostgresStore) OpenProcessAdmission(ctx context.Context, config outbound.ProcessAdmissionConfig) (outbound.ProcessAdmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roleKey, bulkKey, active, valid := processAdmissionRole(config.Role)
	fingerprint, fingerprintErr := ProcessAdmissionFingerprint(config.BudgetFingerprint)
	if !valid || config.CoordinationPoolMax != 2 || config.DataPoolMax < int32(active+1) ||
		config.ReservedConnections <= 0 || int64(config.DBConnectionBudget) < int64(config.DataPoolMax)+2+int64(config.ReservedConnections) ||
		fingerprintErr != nil {
		return nil, fmt.Errorf("%w: invalid bounded process configuration", domain.ErrValidation)
	}
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("%w: bounded profile requires PostgreSQL", errors.ErrUnsupported)
	}
	poolConfig := s.pool.Config()
	dataName := processAdmissionPrefix + string(config.Role) + "/data"
	if poolConfig.MaxConns != config.DataPoolMax || poolConfig.MinConns != 0 || poolConfig.MinIdleConns != 0 ||
		poolConfig.ConnConfig.RuntimeParams["application_name"] != dataName {
		return nil, fmt.Errorf("%w: bounded data pool must be capped and tagged before opening", domain.ErrValidation)
	}
	poolConfig.MaxConns, poolConfig.MinConns, poolConfig.MinIdleConns = 2, 0, 0
	poolConfig.BeforeConnect, poolConfig.AfterConnect = nil, nil
	poolConfig.BeforeAcquire, poolConfig.PrepareConn = nil, nil
	poolConfig.AfterRelease, poolConfig.BeforeClose, poolConfig.ShouldPing = nil, nil, nil
	poolConfig.ConnConfig.Tracer = nil
	poolConfig.ConnConfig.AfterConnect = nil
	poolConfig.ConnConfig.OnNotice, poolConfig.ConnConfig.OnNotification = nil, nil
	poolConfig.ConnConfig.OnPgError = func(_ *pgconn.PgConn, failure *pgconn.PgError) bool {
		return !strings.EqualFold(failure.Severity, "FATAL")
	}
	poolConfig.ConnConfig.BuildContextWatcherHandler = func(connection *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: connection.Conn()}
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = outbound.BoundedDocumentsControlPrefix + string(config.Role) + "/" + fingerprint
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "0"
	poolConfig.ConnConfig.RuntimeParams["lock_timeout"] = "0"
	poolConfig.ConnConfig.RuntimeParams["idle_session_timeout"] = "0"
	poolConfig.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "0"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, processAdmissionFailure(ctx, "coordination pool unavailable")
	}
	control, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, processAdmissionFailure(ctx, "control session unavailable")
	}
	abort := func() {
		processAdmissionDiscard(control)
		pool.Close()
	}
	var maximum, superuserReserve int64
	var reserved *int64
	err = control.QueryRow(ctx, `SELECT current_setting('max_connections')::bigint,
 current_setting('superuser_reserved_connections')::bigint,
 current_setting('reserved_connections', true)::bigint`).Scan(&maximum, &superuserReserve, &reserved)
	if err != nil {
		abort()
		return nil, processAdmissionFailure(ctx, "database connection limits unavailable")
	}
	if reserved == nil {
		abort()
		return nil, fmt.Errorf("%w: bounded profile requires explicit PostgreSQL reserved connection settings", errors.ErrUnsupported)
	}
	if maximum <= 0 || superuserReserve < 0 || *reserved < 0 ||
		int64(config.DBConnectionBudget)+superuserReserve+*reserved > maximum {
		abort()
		return nil, fmt.Errorf("%w: declared connection budget exceeds database nonreserved limit", domain.ErrValidation)
	}
	var locked bool
	if err = control.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::int, $2::int)`, processAdmissionNamespace, roleKey).Scan(&locked); err != nil {
		abort()
		return nil, processAdmissionFailure(ctx, "role registration unavailable")
	}
	if !locked {
		abort()
		return nil, fmt.Errorf("%w: bounded process role already registered", domain.ErrConflict)
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	controller := &postgresProcessAdmission{
		pool: pool, control: control, roleKey: roleKey, bulkKey: bulkKey,
		controlPID: control.Conn().PgConn().PID(), controlUse: make(chan struct{}, 1),
		bulkName:    processAdmissionPrefix + string(config.Role) + "/bulk",
		fingerprint: fingerprint,
		active:      make(chan struct{}, active), bulk: make(chan struct{}, 1),
		ctx: runtimeCtx, cancel: cancel, heartbeat: make(chan struct{}), closed: make(chan struct{}),
	}
	if err = controller.refresh(ctx); err != nil {
		cancel()
		controller.queries.Wait()
		abort()
		return nil, err
	}
	go controller.watchControl()
	go controller.drain()
	return controller, nil
}

func processAdmissionFailure(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", domain.ErrResourceOverloaded, message)
}

func processAdmissionDiscard(connection *pgxpool.Conn) {
	cleanup, cancel := context.WithTimeout(context.Background(), processAdmissionIOTimeout)
	defer cancel()
	_ = connection.Conn().Close(cleanup)
	connection.Release()
}

func processAdmissionUnlock(connection *pgxpool.Conn, key int32) error {
	cleanup, cancel := context.WithTimeout(context.Background(), processAdmissionIOTimeout)
	defer cancel()
	var unlocked bool
	err := connection.QueryRow(cleanup, `SELECT pg_advisory_unlock($1::int, $2::int)`, processAdmissionNamespace, key).Scan(&unlocked)
	if err != nil || !unlocked || cleanup.Err() != nil {
		processAdmissionDiscard(connection)
		return fmt.Errorf("%w: coordination session discarded after uncertain unlock", domain.ErrResourceOverloaded)
	}
	connection.Release()
	return nil
}

func (controller *postgresProcessAdmission) refresh(ctx context.Context) error {
	if err := processAdmissionTake(ctx, controller.controlUse, true); err != nil {
		return err
	}
	controller.mu.Lock()
	if controller.closing || controller.ctx.Err() != nil {
		controller.mu.Unlock()
		<-controller.controlUse
		return processAdmissionFailure(ctx, "control session stopping")
	}
	controller.queries.Add(1)
	controller.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		probe, cancel := context.WithTimeout(context.WithoutCancel(controller.ctx), 500*time.Millisecond)
		err := controller.inspect(probe, controller.control)
		cancel()
		<-controller.controlUse
		controller.queries.Done()
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (controller *postgresProcessAdmission) inspect(ctx context.Context, connection *pgxpool.Conn) error {
	controller.mu.Lock()
	bulkPID, bulkEpoch := controller.bulkPID, controller.bulkEpoch
	controller.mu.Unlock()
	allowed := []string{processAdmissionPrefix + "observer"}
	var controls []string
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		control := outbound.BoundedDocumentsControlPrefix + string(role) + "/" + controller.fingerprint
		controls = append(controls, control)
		allowed = append(allowed, control)
		for _, kind := range []string{"data", "bulk"} {
			allowed = append(allowed, processAdmissionPrefix+string(role)+"/"+kind)
		}
	}
	var own, roster, tagged, bulkOwned bool
	err := connection.QueryRow(ctx, `SELECT
 EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND classid=$1::oid AND objid=$2::oid AND objsubid=2 AND mode='ExclusiveLock' AND granted AND pid=$4::bigint),
 (SELECT count(DISTINCT locks.objid)=3 FROM pg_locks locks JOIN pg_stat_activity sessions ON sessions.pid=locks.pid
  WHERE locks.locktype='advisory' AND locks.database=(SELECT oid FROM pg_database WHERE datname=current_database())
   AND locks.classid=$1::oid AND locks.objid IN (10,11,12) AND locks.objsubid=2 AND locks.mode='ExclusiveLock' AND locks.granted
   AND sessions.usename=session_user AND sessions.application_name=CASE locks.objid
    WHEN 10 THEN $7::text WHEN 11 THEN $8::text WHEN 12 THEN $9::text END),
 NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=session_user
  AND (backend_type IS NULL OR (backend_type='client backend'
   AND (application_name IS NULL OR NOT (application_name=ANY($3::text[])))))),
 ($5::bigint=0 OR EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
  AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=$1::oid
  AND objid=$6::oid AND objsubid=2 AND mode='ExclusiveLock' AND granted AND pid=$5::bigint))`,
		processAdmissionNamespace, controller.roleKey, allowed, int64(controller.controlPID), int64(bulkPID), controller.bulkKey,
		controls[0], controls[1], controls[2]).Scan(&own, &roster, &tagged, &bulkOwned)
	if err != nil {
		return processAdmissionFailure(ctx, "control heartbeat unavailable")
	}
	if !own {
		err := fmt.Errorf("%w: control role lock lost", domain.ErrResourceOverloaded)
		controller.fail(err)
		return err
	}
	if !bulkOwned {
		controller.mu.Lock()
		lost := controller.bulkPID == bulkPID && controller.bulkEpoch == bulkEpoch
		controller.mu.Unlock()
		if lost {
			err := fmt.Errorf("%w: bulk coordination session lost", domain.ErrResourceOverloaded)
			controller.fail(err)
			return err
		}
	}
	if !tagged {
		controller.setReady(false, fmt.Errorf("%w: incompatible execution protocol, schema or budget, or unrecognized same-user database session; drain all roles and use one matching release and budget", domain.ErrConflict))
		return nil
	}
	controller.setReady(roster, nil)
	return nil
}

func (permit *postgresProcessPermit) inspectGrant(ctx context.Context) error {
	if permit.bulkConn != nil {
		return permit.controller.inspect(ctx, permit.bulkConn)
	}
	return permit.controller.refresh(ctx)
}

func (controller *postgresProcessAdmission) setReady(ready bool, err error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closing || controller.ctx.Err() != nil {
		ready = false
	}
	if !ready && controller.readyCancel != nil {
		controller.readyCancel()
	}
	if ready && !controller.ready {
		controller.readyCtx, controller.readyCancel = context.WithCancel(controller.ctx)
	}
	controller.ready, controller.readyErr = ready, err
}

func (controller *postgresProcessAdmission) watchControl() {
	defer close(controller.heartbeat)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-controller.ctx.Done():
			return
		case <-ticker.C:
			probe, cancel := context.WithTimeout(controller.ctx, 500*time.Millisecond)
			err := controller.refresh(probe)
			cancel()
			if err != nil {
				controller.fail(fmt.Errorf("%w: control session lost", domain.ErrResourceOverloaded))
				return
			}
		}
	}
}

func (controller *postgresProcessAdmission) fail(err error) {
	controller.mu.Lock()
	if !controller.closing {
		controller.err, controller.closing, controller.ready = err, true, false
		if controller.readyCancel != nil {
			controller.readyCancel()
		}
	}
	controller.mu.Unlock()
	controller.cancel()
}

func (controller *postgresProcessAdmission) drain() {
	<-controller.ctx.Done()
	controller.fail(controller.ctx.Err())
	<-controller.heartbeat
	controller.permits.Wait()
	controller.queries.Wait()
	controller.closeErr = processAdmissionUnlock(controller.control, controller.roleKey)
	controller.pool.Close()
	close(controller.closed)
}

func (controller *postgresProcessAdmission) Context() context.Context { return controller.ctx }

func (controller *postgresProcessAdmission) Ready() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.ready && !controller.closing && controller.ctx.Err() == nil
}

func (controller *postgresProcessAdmission) Err() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.err != nil {
		return controller.err
	}
	if err := controller.ctx.Err(); err != nil {
		return err
	}
	if controller.readyErr != nil {
		return controller.readyErr
	}
	if !controller.ready {
		return fmt.Errorf("%w: bounded process roster not ready", domain.ErrResourceOverloaded)
	}
	return nil
}

func (controller *postgresProcessAdmission) Close() error {
	controller.fail(context.Canceled)
	<-controller.closed
	return controller.closeErr
}

func (controller *postgresProcessAdmission) Acquire(ctx context.Context, bulk, wait bool) (outbound.ProcessAdmissionPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	controller.mu.Lock()
	if !controller.ready || controller.closing || controller.ctx.Err() != nil {
		controller.mu.Unlock()
		return nil, processAdmissionFailure(ctx, "bounded process roster not ready")
	}
	controller.permits.Add(1)
	controller.mu.Unlock()
	permitCtx, cancel := context.WithCancel(controller.ctx)
	permit := &postgresProcessPermit{
		controller: controller, ctx: permitCtx, cancel: cancel,
		operation: make(chan struct{}, 1), bulkWanted: bulk,
	}
	if err := permit.activate(ctx, wait); err != nil {
		_ = permit.Release()
		return nil, err
	}
	return permit, nil
}

func (permit *postgresProcessPermit) grantContext(ctx context.Context, wait bool) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	controller := permit.controller
	controller.mu.Lock()
	readyCtx := controller.readyCtx
	ready := controller.ready && !controller.closing && controller.ctx.Err() == nil
	controller.mu.Unlock()
	if !ready {
		return nil, nil, processAdmissionFailure(ctx, "bounded process roster not ready")
	}
	var operationCtx context.Context
	var cancel context.CancelFunc
	if wait {
		operationCtx, cancel = context.WithCancel(ctx)
	} else {
		operationCtx, cancel = context.WithTimeout(ctx, processAdmissionIOTimeout)
	}
	stopPermit := context.AfterFunc(permit.ctx, cancel)
	stopReady := context.AfterFunc(readyCtx, cancel)
	return operationCtx, func() { stopPermit(); stopReady(); cancel() }, nil
}

func processAdmissionTake(ctx context.Context, gate chan struct{}, wait bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if wait {
		select {
		case gate <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case gate <- struct{}{}:
		return nil
	default:
		return domain.ErrResourceOverloaded
	}
}

func (permit *postgresProcessPermit) activate(ctx context.Context, wait bool) error {
	grantCtx, cancel, err := permit.grantContext(ctx, wait)
	if err != nil {
		return err
	}
	defer cancel()
	if err = processAdmissionTake(grantCtx, permit.controller.active, wait); err != nil {
		return processAdmissionFailure(ctx, "process active capacity unavailable")
	}
	permit.active = true
	if permit.bulkWanted {
		err = permit.takeBulk(grantCtx, wait)
	}
	if err == nil {
		err = permit.inspectGrant(grantCtx)
	}
	if err == nil && (grantCtx.Err() != nil || !permit.controller.Ready()) {
		err = domain.ErrResourceOverloaded
	}
	if err != nil {
		_ = permit.dropBulk()
		<-permit.controller.active
		permit.active = false
		return processAdmissionFailure(ctx, "process grant unavailable")
	}
	permit.parked = false
	return nil
}

func (permit *postgresProcessPermit) takeBulk(ctx context.Context, wait bool) error {
	controller := permit.controller
	if err := processAdmissionTake(ctx, controller.bulk, wait); err != nil {
		return err
	}
	connection, err := controller.pool.Acquire(ctx)
	if err != nil {
		<-controller.bulk
		return err
	}
	if _, err = connection.Exec(ctx, `SELECT set_config('application_name', $1, false)`, controller.bulkName); err != nil {
		processAdmissionDiscard(connection)
		<-controller.bulk
		return err
	}
	locked := true
	if wait {
		_, err = connection.Exec(ctx, `SELECT pg_advisory_lock($1::int, $2::int)`, processAdmissionNamespace, controller.bulkKey)
	} else {
		err = connection.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::int, $2::int)`, processAdmissionNamespace, controller.bulkKey).Scan(&locked)
	}
	if err != nil || ctx.Err() != nil {
		processAdmissionDiscard(connection)
		<-controller.bulk
		return domain.ErrResourceOverloaded
	}
	if !locked {
		connection.Release()
		<-controller.bulk
		return domain.ErrResourceOverloaded
	}
	permit.bulkConn = connection
	controller.mu.Lock()
	controller.bulkPID = connection.Conn().PgConn().PID()
	controller.bulkEpoch++
	controller.mu.Unlock()
	return nil
}

func (permit *postgresProcessPermit) dropBulk() error {
	if permit.bulkConn == nil {
		return nil
	}
	permit.controller.mu.Lock()
	permit.controller.bulkPID = 0
	permit.controller.bulkEpoch++
	permit.controller.mu.Unlock()
	err := processAdmissionUnlock(permit.bulkConn, permit.controller.bulkKey)
	permit.bulkConn = nil
	<-permit.controller.bulk
	if err != nil {
		permit.controller.fail(err)
	}
	return err
}

func (permit *postgresProcessPermit) begin(ctx context.Context, wait bool) error {
	if err := processAdmissionTake(ctx, permit.operation, wait); err != nil {
		return err
	}
	if permit.finished {
		<-permit.operation
		return fmt.Errorf("%w: process permit released", domain.ErrConflict)
	}
	return nil
}

func (permit *postgresProcessPermit) Upgrade(ctx context.Context, wait bool) error {
	if err := permit.begin(ctx, wait); err != nil {
		return err
	}
	defer func() { <-permit.operation }()
	if permit.parked {
		return fmt.Errorf("%w: process permit parked", domain.ErrConflict)
	}
	grantCtx, cancel, err := permit.grantContext(ctx, wait)
	if err != nil {
		return err
	}
	defer cancel()
	if permit.bulkConn == nil {
		err = permit.takeBulk(grantCtx, wait)
	}
	if err == nil {
		err = permit.inspectGrant(grantCtx)
	}
	if err != nil || grantCtx.Err() != nil || !permit.controller.Ready() {
		_ = permit.dropBulk()
		return processAdmissionFailure(ctx, "process bulk upgrade unavailable")
	}
	permit.bulkWanted = true
	return nil
}

func (permit *postgresProcessPermit) Park(ctx context.Context) error {
	if err := permit.begin(ctx, true); err != nil {
		return err
	}
	defer func() { <-permit.operation }()
	if permit.parked {
		return nil
	}
	err := permit.dropBulk()
	permit.parked = true
	return err
}

func (permit *postgresProcessPermit) Resume(ctx context.Context) error {
	if err := permit.begin(ctx, true); err != nil {
		return err
	}
	defer func() { <-permit.operation }()
	if !permit.parked {
		return nil
	}
	grantCtx, cancel, err := permit.grantContext(ctx, true)
	if err != nil {
		return err
	}
	defer cancel()
	if permit.bulkWanted {
		err = permit.takeBulk(grantCtx, true)
	}
	if err == nil {
		err = permit.inspectGrant(grantCtx)
	}
	if err != nil || grantCtx.Err() != nil || !permit.controller.Ready() {
		_ = permit.dropBulk()
		return processAdmissionFailure(ctx, "process bulk resume unavailable")
	}
	permit.parked = false
	return nil
}

func (permit *postgresProcessPermit) Release() error {
	permit.cancel()
	permit.releaseOnce.Do(func() {
		permit.operation <- struct{}{}
		defer func() { <-permit.operation }()
		permit.finished = true
		permit.releaseErr = permit.dropBulk()
		if permit.active {
			<-permit.controller.active
			permit.active = false
		}
		permit.controller.permits.Done()
	})
	return permit.releaseErr
}
