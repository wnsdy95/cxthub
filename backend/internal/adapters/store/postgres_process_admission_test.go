//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type processAdmissionPGFixture struct {
	ctx      context.Context
	config   *pgxpool.Config
	observer *pgx.Conn
}

func processAdmissionLocalHost(host string) bool {
	if filepath.IsAbs(host) || host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func processAdmissionPG(test *testing.T) *processAdmissionPGFixture {
	test.Helper()
	dsn := os.Getenv("CXT_PROCESS_ADMISSION_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("CXT_TEST_DSN")
	}
	if dsn == "" {
		test.Skip("CXT_TEST_DSN or CXT_PROCESS_ADMISSION_TEST_DSN must explicitly identify an owned local test server")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("invalid owned local process-admission test configuration")
	}
	if !processAdmissionLocalHost(config.ConnConfig.Host) {
		test.Fatal("process-admission fixture refuses a nonlocal server")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !processAdmissionLocalHost(fallback.Host) {
			test.Fatal("process-admission fixture refuses a nonlocal fallback")
		}
	}
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	config.ConnConfig.Tracer = nil
	config.ConnConfig.RuntimeParams["application_name"] = processAdmissionPrefix + "observer"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	test.Cleanup(cancel)
	admin, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal("owned local process-admission test server unavailable")
	}
	database := domain.NewID("cxt_process_admission_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		_ = admin.Close(ctx)
		test.Fatal("could not create fresh owned process-admission test database")
	}
	var observer *pgx.Conn
	test.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if observer != nil {
			_ = observer.Close(cleanup)
		}
		if _, cleanupErr := admin.Exec(cleanup, "DROP DATABASE "+quoted); cleanupErr != nil {
			test.Error("fresh process-admission database cleanup failed")
		}
		_ = admin.Close(cleanup)
	})
	config.ConnConfig.Database = database
	observer, err = pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal("fresh process-admission observer unavailable")
	}
	return &processAdmissionPGFixture{ctx: ctx, config: config, observer: observer}
}

func processAdmissionBudget() executionprofile.Budget {
	budget := executionprofile.Budget{
		Version: 1, Profile: outbound.BoundedDocumentsProfile,
		BaselineActive: 4, BaselineBulk: 2, BaselineConnections: 20, ReservedConnections: 7,
		BaselineCPU: 3, BaselineMemoryBytes: 576 << 20, BaselineSpoolBytes: 192 << 20,
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		entry := executionprofile.RoleBudget{
			Role: role, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2,
			RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 64 << 20,
		}
		if role == outbound.ProcessAPI {
			entry.Active, entry.DataPoolMax = 2, 3
		}
		budget.Roles = append(budget.Roles, entry)
	}
	return budget
}

func processAdmissionConfig(role outbound.ProcessRole) outbound.ProcessAdmissionConfig {
	config, err := processAdmissionBudget().AdmissionConfig(role)
	if err != nil {
		panic(err)
	}
	return config
}

func processAdmissionControlName(role outbound.ProcessRole) string {
	fingerprint, err := ProcessAdmissionFingerprint(processAdmissionConfig(role).BudgetFingerprint)
	if err != nil {
		panic(err)
	}
	return outbound.BoundedDocumentsControlPrefix + string(role) + "/" + fingerprint
}

func (fixture *processAdmissionPGFixture) dataStore(test *testing.T, role outbound.ProcessRole, params map[string]string) *PostgresStore {
	test.Helper()
	config := fixture.config.Copy()
	config.MaxConns = processAdmissionConfig(role).DataPoolMax
	for name, value := range params {
		config.ConnConfig.RuntimeParams[name] = value
	}
	config.ConnConfig.RuntimeParams["application_name"] = processAdmissionPrefix + string(role) + "/data"
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("fresh process-admission data pool unavailable")
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}
}

func (fixture *processAdmissionPGFixture) open(test *testing.T, storage *PostgresStore, role outbound.ProcessRole) *postgresProcessAdmission {
	test.Helper()
	admission, err := storage.OpenProcessAdmission(fixture.ctx, processAdmissionConfig(role))
	if err != nil {
		test.Fatalf("bounded %s registration failed: %v", role, err)
	}
	controller := admission.(*postgresProcessAdmission)
	test.Cleanup(func() { _ = controller.Close() })
	return controller
}

func (fixture *processAdmissionPGFixture) roster(test *testing.T) (*postgresProcessAdmission, *postgresProcessAdmission, *postgresProcessAdmission) {
	test.Helper()
	api := fixture.open(test, fixture.dataStore(test, outbound.ProcessAPI, nil), outbound.ProcessAPI)
	if api.Ready() {
		test.Fatal("incomplete roster became ready")
	}
	mcp := fixture.open(test, fixture.dataStore(test, outbound.ProcessMCP, nil), outbound.ProcessMCP)
	worker := fixture.open(test, fixture.dataStore(test, outbound.ProcessWorker, nil), outbound.ProcessWorker)
	processAdmissionEventually(test, func() bool { return api.Ready() && mcp.Ready() && worker.Ready() })
	return api, mcp, worker
}

func processAdmissionEventually(test *testing.T, predicate func() bool) {
	test.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	test.Fatal("process-admission state did not converge")
}

func processAdmissionPermitPG(test *testing.T, controller *postgresProcessAdmission, bulk bool) outbound.ProcessAdmissionPermit {
	test.Helper()
	permit, err := controller.Acquire(controller.Context(), bulk, false)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = permit.Release() })
	return permit
}

func (fixture *processAdmissionPGFixture) lockCount(test *testing.T, lower, upper int32) int {
	test.Helper()
	var count int
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT count(*) FROM pg_locks
 WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND classid=$1::oid AND objsubid=2 AND objid BETWEEN $2::oid AND $3::oid
 AND mode='ExclusiveLock' AND granted`, processAdmissionNamespace, lower, upper).Scan(&count); err != nil {
		test.Fatal("process-admission lock inspection failed")
	}
	return count
}

func TestPGProcessAdmissionConfiguration(test *testing.T) {
	var storage *PostgresStore
	valid := processAdmissionConfig(outbound.ProcessAPI)
	if _, err := storage.OpenProcessAdmission(context.Background(), valid); !errors.Is(err, errors.ErrUnsupported) {
		test.Fatal("missing PostgreSQL was not unsupported", err)
	}
	for _, mutate := range []func(*outbound.ProcessAdmissionConfig){
		func(config *outbound.ProcessAdmissionConfig) { config.Role = "other" },
		func(config *outbound.ProcessAdmissionConfig) { config.CoordinationPoolMax = 3 },
		func(config *outbound.ProcessAdmissionConfig) { config.DataPoolMax = 2 },
		func(config *outbound.ProcessAdmissionConfig) { config.ReservedConnections = 0 },
		func(config *outbound.ProcessAdmissionConfig) { config.DBConnectionBudget = 0 },
		func(config *outbound.ProcessAdmissionConfig) { config.BudgetFingerprint = "" },
		func(config *outbound.ProcessAdmissionConfig) { config.BudgetFingerprint = strings.Repeat("a", 42) },
		func(config *outbound.ProcessAdmissionConfig) {
			config.BudgetFingerprint = strings.Repeat("a", 43) + "="
		},
	} {
		config := valid
		mutate(&config)
		if _, err := storage.OpenProcessAdmission(context.Background(), config); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid process configuration was accepted", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.OpenProcessAdmission(canceled, valid); !errors.Is(err, context.Canceled) {
		test.Fatal("caller cancellation was lost", err)
	}
}

func TestPGProcessAdmissionMismatchedBudgetsNeverBecomeReady(test *testing.T) {
	fixture := processAdmissionPG(test)
	api := fixture.open(test, fixture.dataStore(test, outbound.ProcessAPI, nil), outbound.ProcessAPI)
	budget := processAdmissionBudget()
	budget.BaselineConnections++
	config, err := budget.AdmissionConfig(outbound.ProcessMCP)
	if err != nil {
		test.Fatal(err)
	}
	admission, err := fixture.dataStore(test, outbound.ProcessMCP, nil).OpenProcessAdmission(fixture.ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	mcp := admission.(*postgresProcessAdmission)
	test.Cleanup(func() { _ = mcp.Close() })
	worker := fixture.open(test, fixture.dataStore(test, outbound.ProcessWorker, nil), outbound.ProcessWorker)
	for _, controller := range []*postgresProcessAdmission{api, mcp, worker} {
		if err := controller.refresh(fixture.ctx); err != nil {
			test.Fatal(err)
		}
		if controller.Ready() || !errors.Is(controller.Err(), domain.ErrConflict) {
			test.Fatal("inconsistent whole-process budgets were accepted", controller.Ready(), controller.Err())
		}
	}
}

func TestPGProcessAdmissionRosterCapsAndWorkerReserve(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, worker := fixture.roster(test)
	if fixture.lockCount(test, 10, 12) != 3 {
		test.Fatal("role registration is not exactly three locks")
	}
	storage := fixture.dataStore(test, outbound.ProcessAPI, nil)
	if _, err := storage.OpenProcessAdmission(fixture.ctx, processAdmissionConfig(outbound.ProcessAPI)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("duplicate API role admitted", err)
	}
	var active []outbound.ProcessAdmissionPermit
	for _, controller := range []*postgresProcessAdmission{api, api, mcp, worker} {
		active = append(active, processAdmissionPermitPG(test, controller, false))
	}
	for _, controller := range []*postgresProcessAdmission{api, mcp, worker} {
		if _, err := controller.Acquire(fixture.ctx, false, false); !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("role active cap bypassed", err)
		}
	}
	if fixture.lockCount(test, 20, 21) != 0 {
		test.Fatal("interactive permit acquired a bulk lock")
	}
	for _, permit := range active {
		if err := permit.Release(); err != nil {
			test.Fatal(err)
		}
	}
	apiBulk := processAdmissionPermitPG(test, api, true)
	if _, err := mcp.Acquire(fixture.ctx, true, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("API and MCP did not share the user bulk lock", err)
	}
	workerBulk := processAdmissionPermitPG(test, worker, true)
	if fixture.lockCount(test, 20, 21) != 2 {
		test.Fatal("user and reserved-worker bulk locks are not exactly two")
	}
	if err := apiBulk.Release(); err != nil {
		test.Fatal(err)
	}
	if err := workerBulk.Release(); err != nil {
		test.Fatal(err)
	}
	_ = processAdmissionPermitPG(test, mcp, true)
}

func TestPGProcessAdmissionUpgradeParkResumeAndCancel(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, _ := fixture.roster(test)
	permit := processAdmissionPermitPG(test, api, false)
	if api.pool.Stat().AcquiredConns() != 1 {
		test.Fatal("interactive permit borrowed a bulk session")
	}
	if err := permit.Upgrade(fixture.ctx, false); err != nil {
		test.Fatal(err)
	}
	if err := permit.Upgrade(fixture.ctx, false); err != nil {
		test.Fatal("repeated upgrade failed", err)
	}
	if api.pool.Stat().AcquiredConns() != 2 {
		test.Fatal("bulk permit did not own exactly one separate session")
	}
	if err := permit.Park(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if err := permit.Park(fixture.ctx); err != nil {
		test.Fatal("repeated park failed", err)
	}
	if api.pool.Stat().AcquiredConns() != 1 {
		test.Fatal("park retained the bulk connection and can deadlock pool2")
	}
	if len(api.active) != 1 {
		test.Fatal("park released retained active ownership before final cleanup")
	}
	other := processAdmissionPermitPG(test, mcp, true)
	deadline, cancel := context.WithTimeout(fixture.ctx, 75*time.Millisecond)
	err := permit.Resume(deadline)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("resume did not preserve caller deadline", err)
	}
	processAdmissionEventually(test, func() bool {
		var live bool
		err := fixture.observer.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND usename=session_user AND application_name=$1)`,
			processAdmissionPrefix+"api/bulk").Scan(&live)
		return err == nil && !live && api.pool.Stat().AcquiredConns() == 1
	})
	reader := processAdmissionPermitPG(test, api, false)
	if _, err := api.Acquire(fixture.ctx, false, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("parked ownership bypassed the role active limit", err)
	}
	if err := reader.Upgrade(fixture.ctx, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("try upgrade waited or bypassed the shared bulk lock", err)
	}
	if err := reader.Release(); err != nil {
		test.Fatal(err)
	}
	if err := other.Release(); err != nil {
		test.Fatal(err)
	}
	if err := permit.Resume(fixture.ctx); err != nil {
		test.Fatal("parked permit could not reacquire a fresh bulk session", err)
	}
	if err := permit.Release(); err != nil {
		test.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		test.Fatal("release is not idempotent", err)
	}
	if fixture.lockCount(test, 20, 21) != 0 {
		test.Fatal("repeated upgrade or release leaked a session lock")
	}
	if err := permit.Upgrade(fixture.ctx, false); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("released permit upgraded", err)
	}
}

func TestPGProcessAdmissionCanceledAcquiredReplyDoesNotLeak(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, _ := fixture.roster(test)
	if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_lock($1::int,20)`, processAdmissionNamespace); err != nil {
		test.Fatal("observer lock setup failed")
	}
	waiting, cancel := context.WithCancel(fixture.ctx)
	test.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		permit, err := mcp.Acquire(waiting, true, true)
		if permit != nil {
			_ = permit.Release()
		}
		result <- err
	}()
	processAdmissionEventually(test, func() bool {
		var pending bool
		err := fixture.observer.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
 WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND classid=$1::oid AND objid=20 AND objsubid=2 AND NOT granted)`, processAdmissionNamespace).Scan(&pending)
		return err == nil && pending
	})
	cancel()
	if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_unlock($1::int,20)`, processAdmissionNamespace); err != nil {
		test.Fatal("observer lock cleanup failed")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			test.Fatal("canceled lock reply became a grant", err)
		}
	case <-fixture.ctx.Done():
		test.Fatal("canceled blocking admission did not return")
	}
	processAdmissionEventually(test, func() bool { return fixture.lockCount(test, 20, 20) == 0 })
	_ = processAdmissionPermitPG(test, api, true)
}

func TestPGProcessAdmissionWaiterRechecksLiveRoster(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, _ := fixture.roster(test)
	active := processAdmissionPermitPG(test, api, true)
	result := make(chan error, 1)
	go func() {
		permit, err := mcp.Acquire(fixture.ctx, true, true)
		if permit != nil {
			_ = permit.Release()
		}
		result <- err
	}()
	processAdmissionEventually(test, func() bool {
		var waiting bool
		err := fixture.observer.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
 WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND classid=$1::oid AND objid=20 AND objsubid=2 AND NOT granted)`, processAdmissionNamespace).Scan(&waiting)
		return err == nil && waiting
	})
	var terminated bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname=current_database() AND usename=session_user AND application_name=$1`,
		processAdmissionControlName(outbound.ProcessAPI)).Scan(&terminated); err != nil || !terminated {
		test.Fatal("could not terminate owned queued-grant peer")
	}
	if err := active.Release(); err != nil {
		test.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("waiter granted from a stale cached roster", err)
		}
	case <-fixture.ctx.Done():
		test.Fatal("waiter did not stop after peer control loss")
	}
}

func TestPGProcessAdmissionCloseDrainsBeforeRoleHandoff(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, _, _ := fixture.roster(test)
	permit := processAdmissionPermitPG(test, api, true)
	closed := make(chan error, 1)
	go func() { closed <- api.Close() }()
	select {
	case <-api.Context().Done():
	case <-time.After(time.Second):
		test.Fatal("close waited for permits before canceling the runtime")
	}
	if api.Ready() {
		test.Fatal("closing controller remained ready")
	}
	select {
	case <-closed:
		test.Fatal("close returned while a permit still owns bulk work")
	default:
	}
	if fixture.lockCount(test, 10, 10) != 1 {
		test.Fatal("role handed off before active work drained")
	}
	storage := fixture.dataStore(test, outbound.ProcessAPI, nil)
	if _, err := storage.OpenProcessAdmission(fixture.ctx, processAdmissionConfig(outbound.ProcessAPI)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("duplicate role acquired during drain", err)
	}
	if _, err := api.Acquire(fixture.ctx, false, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("closing controller granted new work", err)
	}
	if err := permit.Release(); err != nil {
		test.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			test.Fatal(err)
		}
	case <-fixture.ctx.Done():
		test.Fatal("close did not finish after release")
	}
	replacement := fixture.open(test, storage, outbound.ProcessAPI)
	processAdmissionEventually(test, replacement.Ready)
}

func TestPGProcessAdmissionPeerAndOwnControlLoss(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, worker := fixture.roster(test)
	if err := mcp.Close(); err != nil {
		test.Fatal(err)
	}
	processAdmissionEventually(test, func() bool { return !api.Ready() && !worker.Ready() })
	if api.Context().Err() != nil || worker.Context().Err() != nil {
		test.Fatal("missing peer crashed another healthy controller")
	}
	if _, err := api.Acquire(fixture.ctx, false, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("missing peer did not stop admission", err)
	}
	_ = fixture.open(test, fixture.dataStore(test, outbound.ProcessMCP, nil), outbound.ProcessMCP)
	processAdmissionEventually(test, func() bool { return api.Ready() && worker.Ready() })
	permit := processAdmissionPermitPG(test, api, true)
	var terminated bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname=current_database() AND usename=session_user AND application_name=$1`,
		processAdmissionControlName(outbound.ProcessAPI)).Scan(&terminated); err != nil || !terminated {
		test.Fatal("could not terminate owned control session")
	}
	select {
	case <-api.Context().Done():
	case <-time.After(time.Second):
		test.Fatal("own control failure did not cancel runtime within one second")
	}
	if api.Ready() || !errors.Is(api.Err(), domain.ErrResourceOverloaded) {
		test.Fatal("own control loss was not permanently fail-closed", api.Err())
	}
	if _, err := api.Acquire(fixture.ctx, true, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("failed controller granted new work", err)
	}
	if err := permit.Release(); err != nil {
		test.Fatal(err)
	}
}

func TestPGProcessAdmissionUnknownSessionFailsClosed(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, mcp, worker := fixture.roster(test)
	for _, application := range []string{"", "legacy-api", processAdmissionPrefix + "api/data/extra"} {
		config := fixture.config.ConnConfig.Copy()
		config.RuntimeParams["application_name"] = application
		unknown, err := pgx.ConnectConfig(fixture.ctx, config)
		if err != nil {
			test.Fatal("could not open owned unknown-session fixture")
		}
		processAdmissionEventually(test, func() bool { return !api.Ready() && !mcp.Ready() && !worker.Ready() })
		if _, err := worker.Acquire(fixture.ctx, false, false); !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("unknown same-login session did not stop admission", err)
		}
		if !errors.Is(api.Err(), domain.ErrConflict) || api.Context().Err() != nil {
			test.Fatal("tag check did not preserve healthy control session", api.Err())
		}
		_ = unknown.Close(fixture.ctx)
		processAdmissionEventually(test, func() bool { return api.Ready() && mcp.Ready() && worker.Ready() })
	}
}

func TestPGProcessAdmissionBulkSessionLossFailsClosed(test *testing.T) {
	fixture := processAdmissionPG(test)
	api, _, _ := fixture.roster(test)
	permit := processAdmissionPermitPG(test, api, true)
	var terminated bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname=current_database() AND usename=session_user AND application_name=$1`,
		processAdmissionPrefix+"api/bulk").Scan(&terminated); err != nil || !terminated {
		test.Fatal("could not terminate owned bulk session")
	}
	select {
	case <-api.Context().Done():
	case <-time.After(time.Second):
		test.Fatal("bulk session loss did not cancel the runtime")
	}
	if api.Ready() || !errors.Is(api.Err(), domain.ErrResourceOverloaded) {
		test.Fatal("lost bulk ownership was not fail-closed", api.Err())
	}
	if fixture.lockCount(test, 10, 10) != 1 {
		test.Fatal("bulk-loss cancellation dropped a healthy role before permit cleanup")
	}
	if err := permit.Release(); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("lost session release did not report uncertain unlock", err)
	}
	if err := permit.Release(); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("uncertain release result was not idempotent", err)
	}
}

func TestPGProcessAdmissionLiveConnectionBudgetAndDataPool(test *testing.T) {
	fixture := processAdmissionPG(test)
	var usable int32
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT current_setting('max_connections')::int
 -current_setting('superuser_reserved_connections')::int-current_setting('reserved_connections')::int`).Scan(&usable); err != nil {
		test.Fatal("live nonreserved connection limit unavailable")
	}
	storage := fixture.dataStore(test, outbound.ProcessAPI, nil)
	tooLarge := processAdmissionConfig(outbound.ProcessAPI)
	tooLarge.DBConnectionBudget = usable + 1
	if _, err := storage.OpenProcessAdmission(fixture.ctx, tooLarge); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("database reserved connections were counted as available budget", err)
	}
	for _, change := range []func(*pgxpool.Config){
		func(config *pgxpool.Config) { config.MaxConns++ },
		func(config *pgxpool.Config) { config.MinConns = 1 },
		func(config *pgxpool.Config) { config.MinIdleConns = 1 },
		func(config *pgxpool.Config) { config.ConnConfig.RuntimeParams["application_name"] = "legacy" },
	} {
		config := storage.pool.Config()
		change(config)
		pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
		if err != nil {
			test.Fatal("invalid data-pool fixture could not be constructed")
		}
		_, err = (&PostgresStore{pool: pool}).OpenProcessAdmission(fixture.ctx, processAdmissionConfig(outbound.ProcessAPI))
		pool.Close()
		if !errors.Is(err, domain.ErrValidation) {
			test.Fatal("adapter silently changed or accepted an uncapped data pool", err)
		}
	}
	if fixture.lockCount(test, 10, 12) != 0 {
		test.Fatal("rejected configuration leaked a role registration")
	}
}

func TestPGProcessAdmissionPreservesStartupIdentity(test *testing.T) {
	fixture := processAdmissionPG(test)
	var superuser bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=session_user`).Scan(&superuser); err != nil || !superuser {
		test.Skip("startup SET ROLE regression requires an owned local superuser fixture")
	}
	role := domain.NewID("cxt_process_role_")
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := fixture.observer.Exec(fixture.ctx, "CREATE ROLE "+quoted+" NOLOGIN"); err != nil {
		test.Fatal("owned startup-role fixture creation failed")
	}
	test.Cleanup(func() {
		if _, err := fixture.observer.Exec(fixture.ctx, "DROP ROLE "+quoted); err != nil {
			test.Error("owned startup-role fixture cleanup failed")
		}
	})
	if _, err := fixture.observer.Exec(fixture.ctx, "GRANT pg_read_all_stats TO "+quoted); err != nil {
		test.Fatal("owned startup-role statistics grant failed")
	}
	var controllers []*postgresProcessAdmission
	for _, processRole := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		storage := fixture.dataStore(test, processRole, map[string]string{
			"role": role, "search_path": "pg_catalog", "synchronous_commit": "on",
		})
		controller := fixture.open(test, storage, processRole)
		controllers = append(controllers, controller)
		config := controller.pool.Config()
		if config.ConnConfig.RuntimeParams["role"] != role || config.ConnConfig.RuntimeParams["search_path"] != "pg_catalog" ||
			config.ConnConfig.RuntimeParams["synchronous_commit"] != "on" || config.MaxConns != 2 || config.MinConns != 0 || config.MinIdleConns != 0 {
			test.Fatal("coordination configuration dropped identity/security startup parameters")
		}
		connection, err := controller.pool.Acquire(fixture.ctx)
		if err != nil {
			test.Fatal("owned startup-role inspection connection unavailable")
		}
		var current, session, searchPath, synchronous string
		err = connection.QueryRow(fixture.ctx, `SELECT current_user, session_user, current_setting('search_path'), current_setting('synchronous_commit')`).Scan(
			&current, &session, &searchPath, &synchronous)
		connection.Release()
		if err != nil || current != role || current == session || searchPath != "pg_catalog" || synchronous != "on" {
			test.Fatal("coordination connection elevated or changed startup identity")
		}
		if storage.pool.Config().ConnConfig.RuntimeParams["application_name"] != processAdmissionPrefix+string(processRole)+"/data" {
			test.Fatal("adapter mutated the original data pool")
		}
	}
	processAdmissionEventually(test, func() bool {
		return controllers[0].Ready() && controllers[1].Ready() && controllers[2].Ready()
	})
}

func TestPGProcessAdmissionCoordinationCopyDropsDataHooks(test *testing.T) {
	fixture := processAdmissionPG(test)
	config := fixture.config.Copy()
	config.MaxConns = 3
	config.ConnConfig.RuntimeParams["application_name"] = processAdmissionPrefix + "api/data"
	config.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	config.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	config.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("data-only before-connect hook") }
	config.AfterConnect = func(context.Context, *pgx.Conn) error { return errors.New("data-only after-connect hook") }
	config.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) { return false, errors.New("data-only prepare hook") }
	config.ConnConfig.AfterConnect = func(context.Context, *pgconn.PgConn) error { return errors.New("data-only low-level hook") }
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("data-only hook fixture unavailable")
	}
	test.Cleanup(pool.Close)
	controller := fixture.open(test, &PostgresStore{pool: pool}, outbound.ProcessAPI)
	coord := controller.pool.Config()
	original := pool.Config()
	if coord.BeforeConnect != nil || coord.AfterConnect != nil || coord.PrepareConn != nil || coord.ConnConfig.AfterConnect != nil ||
		coord.ConnConfig.Tracer != nil || coord.ConnConfig.RuntimeParams["search_path"] != "pg_catalog" ||
		coord.ConnConfig.RuntimeParams["synchronous_commit"] != "on" || original.BeforeConnect == nil || original.AfterConnect == nil ||
		original.PrepareConn == nil || original.ConnConfig.AfterConnect == nil || original.MaxConns != 3 {
		test.Fatal("coordination copy retained data hooks or changed the data pool")
	}
}

func TestPGProcessAdmissionSanitizesErrors(test *testing.T) {
	fixture := processAdmissionPG(test)
	config := fixture.config.Copy()
	config.MaxConns = 3
	config.ConnConfig.RuntimeParams["application_name"] = processAdmissionPrefix + "api/data"
	config.ConnConfig.Password = "must-never-appear-in-errors"
	config.ConnConfig.RuntimeParams["role"] = "absent_cxt_process_admission_role"
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("sanitized-error fixture pool unavailable")
	}
	defer pool.Close()
	_, err = (&PostgresStore{pool: pool}).OpenProcessAdmission(fixture.ctx, processAdmissionConfig(outbound.ProcessAPI))
	if !errors.Is(err, domain.ErrResourceOverloaded) || strings.Contains(fmt.Sprint(err), config.ConnConfig.Password) ||
		strings.Contains(fmt.Sprint(err), "absent_cxt_process_admission_role") {
		test.Fatal("connection error was not sanitized")
	}
}
