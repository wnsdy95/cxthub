//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func processAdmissionLegacyControlPG(test *testing.T, fixture *processAdmissionPGFixture, role outbound.ProcessRole, roleKey int32, identity string) *pgx.Conn {
	test.Helper()
	config := fixture.config.ConnConfig.Copy()
	config.RuntimeParams["application_name"] = "cxthub/bd1/" + string(role) + "/" + identity
	legacy, err := pgx.ConnectConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("legacy protocol witness connection unavailable", err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), processAdmissionIOTimeout)
		defer cancel()
		_ = legacy.Close(cleanup)
	})
	var locked bool
	if err := legacy.QueryRow(fixture.ctx, `SELECT pg_try_advisory_lock(1129862210, $1::int)`, roleKey).Scan(&locked); err != nil || !locked {
		test.Fatal("legacy protocol witness did not acquire its original role lock", locked, err)
	}
	return legacy
}

func processAdmissionProtocolConflictedPG(test *testing.T, fixture *processAdmissionPGFixture, controllers ...*postgresProcessAdmission) {
	test.Helper()
	for _, controller := range controllers {
		if err := controller.refresh(fixture.ctx); err != nil {
			test.Fatal("legacy protocol witness broke a healthy control connection", err)
		}
		if controller.Ready() || !errors.Is(controller.Err(), domain.ErrConflict) || controller.Context().Err() != nil {
			test.Fatal("same-budget legacy protocol witness was not a recoverable conflict", controller.Ready(), controller.Err())
		}
		for _, bulk := range []bool{false, true} {
			permit, err := controller.Acquire(fixture.ctx, bulk, false)
			if permit != nil {
				_ = permit.Release()
			}
			if permit != nil || !errors.Is(err, domain.ErrResourceOverloaded) {
				test.Fatal("legacy protocol conflict admitted new work", bulk, err)
			}
		}
	}
}

func processAdmissionProtocolRecoveredPG(test *testing.T, fixture *processAdmissionPGFixture, storage map[outbound.ProcessRole]*PostgresStore, controllers map[outbound.ProcessRole]*postgresProcessAdmission) {
	test.Helper()
	roles := []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker}
	processAdmissionEventually(test, func() bool {
		return controllers[outbound.ProcessAPI].Ready() && controllers[outbound.ProcessMCP].Ready() && controllers[outbound.ProcessWorker].Ready()
	})
	var exactRoster bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT count(*)=3 AND count(DISTINCT locks.objid)=3
 FROM pg_locks locks JOIN pg_stat_activity sessions ON sessions.pid=locks.pid
 WHERE locks.locktype='advisory' AND locks.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND locks.classid=1129862210::oid AND locks.objid IN (10,11,12) AND locks.objsubid=2
 AND locks.mode='ExclusiveLock' AND locks.granted AND sessions.usename=session_user
 AND sessions.application_name=CASE locks.objid WHEN 10 THEN $1::text WHEN 11 THEN $2::text WHEN 12 THEN $3::text END`,
		processAdmissionControlName(outbound.ProcessAPI), processAdmissionControlName(outbound.ProcessMCP), processAdmissionControlName(outbound.ProcessWorker)).Scan(&exactRoster); err != nil || !exactRoster {
		test.Fatal("recovery did not restore the exact current roster on the original role locks", exactRoster, err)
	}
	if fixture.lockCount(test, 10, 12) != 3 {
		test.Fatal("recovery changed the role-lock namespace or duplicated roles")
	}
	var active []outbound.ProcessAdmissionPermit
	for _, role := range roles {
		controller := controllers[role]
		activeCap, dataPoolCap := 1, int32(2)
		if role == outbound.ProcessAPI {
			activeCap, dataPoolCap = 2, 3
		}
		coordination := controller.pool.Config()
		data := storage[role].pool.Config()
		if controller.Err() != nil || controller.Context().Err() != nil || cap(controller.active) != activeCap || cap(controller.bulk) != 1 ||
			coordination.MaxConns != 2 || coordination.MinConns != 0 || coordination.MinIdleConns != 0 ||
			data.MaxConns != dataPoolCap || data.MinConns != 0 || data.MinIdleConns != 0 {
			test.Fatal("protocol recovery changed healthy control state or original admission/pool caps", role, controller.Err())
		}
		duplicate, err := storage[role].OpenProcessAdmission(fixture.ctx, processAdmissionConfig(role))
		if duplicate != nil {
			_ = duplicate.Close()
		}
		if duplicate != nil || !errors.Is(err, domain.ErrConflict) {
			test.Fatal("current protocol recovery admitted a duplicate role", role, err)
		}
		for granted := 0; granted < activeCap; granted++ {
			active = append(active, processAdmissionPermitPG(test, controller, false))
		}
		permit, err := controller.Acquire(fixture.ctx, false, false)
		if permit != nil {
			_ = permit.Release()
		}
		if permit != nil || !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("recovered roster exceeded the original per-role active cap", role, err)
		}
	}
	if len(active) != 4 || fixture.lockCount(test, 20, 21) != 0 {
		test.Fatal("recovered interactive grants changed the original active or bulk caps")
	}
	for _, permit := range active {
		if err := permit.Release(); err != nil {
			test.Fatal(err)
		}
	}
	apiBulk := processAdmissionPermitPG(test, controllers[outbound.ProcessAPI], true)
	workerBulk := processAdmissionPermitPG(test, controllers[outbound.ProcessWorker], true)
	permit, err := controllers[outbound.ProcessMCP].Acquire(fixture.ctx, true, false)
	if permit != nil {
		_ = permit.Release()
	}
	if permit != nil || !errors.Is(err, domain.ErrResourceOverloaded) || fixture.lockCount(test, 20, 21) != 2 {
		test.Fatal("recovered user/worker bulk grants bypassed the original shared and reserved locks", err)
	}
	if err := apiBulk.Release(); err != nil {
		test.Fatal(err)
	}
	mcpBulk := processAdmissionPermitPG(test, controllers[outbound.ProcessMCP], true)
	if err := mcpBulk.Release(); err != nil {
		test.Fatal(err)
	}
	if err := workerBulk.Release(); err != nil {
		test.Fatal(err)
	}
	if fixture.lockCount(test, 20, 21) != 0 || fixture.lockCount(test, 10, 12) != 3 {
		test.Fatal("recovered useful grants leaked bulk ownership or lost role registration")
	}
}

func TestPGProcessAdmissionLegacyBudgetControlRejectsCurrentPeers(test *testing.T) {
	for _, legacyProtocol := range []string{"raw-budget", "v6"} {
		for _, legacyRole := range []struct {
			role    outbound.ProcessRole
			roleKey int32
		}{
			{role: outbound.ProcessAPI, roleKey: 10},
			{role: outbound.ProcessMCP, roleKey: 11},
			{role: outbound.ProcessWorker, roleKey: 12},
		} {
			test.Run(legacyProtocol+"/"+string(legacyRole.role), func(test *testing.T) {
				fixture := processAdmissionPG(test)
				identity := processAdmissionConfig(legacyRole.role).BudgetFingerprint
				if legacyProtocol == "v6" {
					witness := sha256.Sum256([]byte("cxthub/process-admission/v6\x00" + releasedMigrationCatalogSHA256 + "\x00" + identity))
					identity = base64.RawURLEncoding.EncodeToString(witness[:])
				}
				legacy := processAdmissionLegacyControlPG(test, fixture, legacyRole.role, legacyRole.roleKey, identity)
				storage := make(map[outbound.ProcessRole]*PostgresStore)
				controllers := make(map[outbound.ProcessRole]*postgresProcessAdmission)
				var peers []*postgresProcessAdmission
				for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
					if processAdmissionConfig(role).BudgetFingerprint != processAdmissionConfig(legacyRole.role).BudgetFingerprint {
						test.Fatal("legacy witness did not use the identical whole-process budget")
					}
					storage[role] = fixture.dataStore(test, role, nil)
					if role != legacyRole.role {
						controllers[role] = fixture.open(test, storage[role], role)
						if controllers[role].Ready() {
							test.Fatal("current peer became ready alongside a same-budget legacy role", role)
						}
						peers = append(peers, controllers[role])
					}
				}
				processAdmissionProtocolConflictedPG(test, fixture, peers...)
				if fixture.lockCount(test, 10, 12) != 3 || fixture.lockCount(test, 20, 21) != 0 {
					test.Fatal("mixed protocol roster changed original role locks or granted bulk work")
				}
				duplicate, err := storage[legacyRole.role].OpenProcessAdmission(fixture.ctx, processAdmissionConfig(legacyRole.role))
				if duplicate != nil {
					_ = duplicate.Close()
				}
				if duplicate != nil || !errors.Is(err, domain.ErrConflict) {
					test.Fatal("legacy role lock did not exclude a current duplicate", err)
				}
				if err := legacy.Close(fixture.ctx); err != nil {
					test.Fatal("legacy control witness did not drain", err)
				}
				if fixture.lockCount(test, 10, 12) != 2 {
					test.Fatal("legacy control drain did not release exactly its original role lock")
				}
				controllers[legacyRole.role] = fixture.open(test, storage[legacyRole.role], legacyRole.role)
				processAdmissionProtocolRecoveredPG(test, fixture, storage, controllers)
			})
		}
	}
}

func TestPGProcessAdmissionLegacyPeerReplacementCancelsActiveUpgrade(test *testing.T) {
	fixture := processAdmissionPG(test)
	storage := make(map[outbound.ProcessRole]*PostgresStore)
	controllers := make(map[outbound.ProcessRole]*postgresProcessAdmission)
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		storage[role] = fixture.dataStore(test, role, nil)
		controllers[role] = fixture.open(test, storage[role], role)
	}
	api, mcp, worker := controllers[outbound.ProcessAPI], controllers[outbound.ProcessMCP], controllers[outbound.ProcessWorker]
	processAdmissionEventually(test, func() bool { return api.Ready() && mcp.Ready() && worker.Ready() })
	active := processAdmissionPermitPG(test, api, false)
	blocker := processAdmissionPermitPG(test, mcp, true)
	upgradeCtx, cancel := context.WithCancel(fixture.ctx)
	test.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- active.Upgrade(upgradeCtx, true) }()
	processAdmissionEventually(test, func() bool {
		var waiting bool
		err := fixture.observer.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks locks
 JOIN pg_stat_activity sessions ON sessions.pid=locks.pid
 WHERE locks.locktype='advisory' AND locks.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND locks.classid=1129862210::oid AND locks.objid=20 AND locks.objsubid=2 AND NOT locks.granted
 AND sessions.usename=session_user AND sessions.application_name=$1)`, processAdmissionPrefix+"api/bulk").Scan(&waiting)
		return err == nil && waiting
	})
	if err := worker.Close(); err != nil {
		test.Fatal("current worker control did not drain before legacy replacement", err)
	}
	legacy := processAdmissionLegacyControlPG(test, fixture, outbound.ProcessWorker, 12, processAdmissionConfig(outbound.ProcessWorker).BudgetFingerprint)
	processAdmissionProtocolConflictedPG(test, fixture, api, mcp)
	select {
	case err := <-result:
		if !errors.Is(err, domain.ErrResourceOverloaded) || upgradeCtx.Err() != nil {
			test.Fatal("incompatible peer transition did not cancel the active permit's blocked upgrade independently of its caller", err, upgradeCtx.Err())
		}
	case <-time.After(2 * time.Second):
		test.Fatal("active permit upgrade did not stop within the original convergence bound")
	}
	if err := active.Upgrade(fixture.ctx, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("active permit upgraded against the incompatible replacement", err)
	}
	if len(api.active) != 1 || fixture.lockCount(test, 10, 12) != 3 || fixture.lockCount(test, 20, 21) != 1 {
		test.Fatal("canceled upgrade lost retained active/role ownership or disturbed the existing bulk grant")
	}
	if err := blocker.Release(); err != nil {
		test.Fatal(err)
	}
	if err := active.Release(); err != nil {
		test.Fatal(err)
	}
	if err := legacy.Close(fixture.ctx); err != nil {
		test.Fatal("incompatible replacement control did not drain", err)
	}
	controllers[outbound.ProcessWorker] = fixture.open(test, storage[outbound.ProcessWorker], outbound.ProcessWorker)
	processAdmissionProtocolRecoveredPG(test, fixture, storage, controllers)
}
