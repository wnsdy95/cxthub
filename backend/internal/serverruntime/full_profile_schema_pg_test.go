//go:build postgres

package serverruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func fullProfileSchemaPGLock(test *testing.T, observer *pgx.Conn) func() {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := observer.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		test.Fatal("hold the actual migration lock in the owned database", err)
	}
	locked := true
	unlock := func() {
		if !locked {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var released bool
		if err := observer.QueryRow(cleanup, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`).Scan(&released); err != nil || !released {
			test.Error("release only the fixture's migration lock", released, err)
			return
		}
		locked = false
	}
	test.Cleanup(unlock)
	return unlock
}

func fullProfileSchemaPGWaitBlocked(test *testing.T, observer *pgx.Conn, names []string) {
	test.Helper()
	executionRootWait(test, func(ctx context.Context) bool {
		var blocked int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_locks waiting
 JOIN pg_locks held USING(locktype,database,classid,objid,objsubid)
 JOIN pg_stat_activity activity ON activity.pid=waiting.pid
 WHERE waiting.locktype='advisory' AND waiting.mode='ShareLock' AND NOT waiting.granted AND held.granted
 AND held.pid=pg_backend_pid() AND activity.datname=current_database()
 AND activity.application_name=ANY($1::text[])`, names).Scan(&blocked)
		return err == nil && blocked == len(names)
	})
}

func fullProfileSchemaPGNoAdmission(test *testing.T, observer *pgx.Conn) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var absent bool
	if err := observer.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name LIKE $1)
 AND NOT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
  AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND classid::bigint=1129862210 AND objsubid=2)`, outbound.BoundedDocumentsControlPrefix+"%").Scan(&absent); err != nil || !absent {
		test.Fatal("schema startup acquired or retained process control sessions or admission locks", absent, err)
	}
}

func fullProfileSchemaPGCatalog(test *testing.T, observer *pgx.Conn) string {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var catalog string
	if err := observer.QueryRow(ctx, `SELECT COALESCE(jsonb_object_agg(kind||':'||identity,definition),'{}'::jsonb)::text FROM (
 SELECT 'relation' AS kind,relname::text AS identity,
  jsonb_build_array(relkind,relpersistence,relrowsecurity,relforcerowsecurity,
   CASE WHEN relkind IN ('v','m') THEN pg_get_viewdef(oid) END)::text AS definition
 FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','p','v','m','S','f')
 UNION ALL
 SELECT 'column',relation.relname||'.'||attribute.attnum,
  jsonb_build_array(attribute.attname,format_type(attribute.atttypid,attribute.atttypmod),attribute.attnotnull,
   attribute.attidentity,attribute.attgenerated,pg_get_expr(defaults.adbin,defaults.adrelid))::text
 FROM pg_attribute attribute JOIN pg_class relation ON relation.oid=attribute.attrelid
 LEFT JOIN pg_attrdef defaults ON defaults.adrelid=attribute.attrelid AND defaults.adnum=attribute.attnum
 WHERE relation.relnamespace='public'::regnamespace AND attribute.attnum>0 AND NOT attribute.attisdropped
 UNION ALL
 SELECT 'constraint',relation.relname||'.'||constraint_row.conname,pg_get_constraintdef(constraint_row.oid)
 FROM pg_constraint constraint_row JOIN pg_class relation ON relation.oid=constraint_row.conrelid
 WHERE relation.relnamespace='public'::regnamespace
 UNION ALL
 SELECT 'index',indexrelid::regclass::text,jsonb_build_array(pg_get_indexdef(indexrelid),indisvalid,indisready)::text
 FROM pg_index JOIN pg_class relation ON relation.oid=indrelid WHERE relation.relnamespace='public'::regnamespace
 UNION ALL
 SELECT 'function',oid::regprocedure::text,pg_get_functiondef(oid)
 FROM pg_proc WHERE pronamespace='public'::regnamespace AND prokind IN ('f','p')
 UNION ALL
 SELECT 'trigger',relation.relname||'.'||trigger_row.tgname,
  jsonb_build_array(pg_get_triggerdef(trigger_row.oid),trigger_row.tgenabled)::text
 FROM pg_trigger trigger_row JOIN pg_class relation ON relation.oid=trigger_row.tgrelid
 WHERE relation.relnamespace='public'::regnamespace AND NOT trigger_row.tgisinternal
 ) observed_schema`).Scan(&catalog); err != nil {
		test.Fatal("observe the current schema without changing migration or business state", err)
	}
	return catalog
}

func fullProfileSchemaPGHistory(test *testing.T, observer *pgx.Conn, directory string) []string {
	test.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	expected := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		expected[entry.Name()] = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	if len(expected) == 0 {
		test.Fatal("current required migration source is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := observer.Query(ctx, `SELECT version,checksum,applied_at FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var history []string
	for rows.Next() {
		var version, checksum string
		var appliedAt time.Time
		if err := rows.Scan(&version, &checksum, &appliedAt); err != nil {
			test.Fatal(err)
		}
		if want, present := expected[version]; !present || checksum != want || appliedAt.IsZero() {
			test.Fatal("migration history is duplicated, foreign, incomplete or inconsistent with its actual source", version)
		}
		delete(expected, version)
		history = append(history, version+":"+checksum+":"+appliedAt.UTC().Format(time.RFC3339Nano))
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	if len(expected) != 0 {
		test.Fatal("startup returned with missing current migrations", len(expected))
	}
	return history
}

func TestFullProfilePGSchemaVerificationLockCancellationClosesActualRoots(test *testing.T) {
	fixture := newWave48PGFixture(test)
	for _, role := range []outbound.ProcessRole{outbound.ProcessWorker, outbound.ProcessAPI, outbound.ProcessMCP} {
		test.Run(string(role), func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			dsn, observer := executionRootPostgresFixture(test)
			executionRootPostgresEnvironment(test, dsn)
			test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedWorkersProfile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, fixture.budget))
			priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			priorSpools := fixture.storage.TransientSpoolBudgetUsage()
			before := fullProfileSchemaPGCatalog(test, observer)
			history := fullProfileSchemaPGHistory(test, observer, fixture.migrations)
			if before == "{}" {
				test.Fatal("verification cancellation requires an explicitly migrated owned database")
			}
			fullProfileSchemaPGLock(test, observer)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var opened bool
			var startupErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				if role == outbound.ProcessWorker {
					worker, err := wave48PGOpenWorker(ctx)
					opened, startupErr = worker != nil, err
					if worker != nil {
						startupErr = errors.Join(startupErr, worker.Close())
					}
					return
				}
				open := Open
				if role == outbound.ProcessMCP {
					open = OpenMCP
				}
				root, err := open(ctx, "127.0.0.1:0", data, true)
				opened, startupErr = root != nil, err
				if root != nil {
					root.Close()
				}
			}()
			test.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					test.Error("actual root startup did not join after cancellation")
				}
			})
			name := "cxthub/" + outbound.BoundedDocumentsProfile + "/" + string(role) + "/data"
			fullProfileSchemaPGWaitBlocked(test, observer, []string{name})
			select {
			case <-done:
				test.Fatal("actual full root returned while schema verification was locked", opened, startupErr)
			default:
			}
			fullProfileSchemaPGNoAdmission(test, observer)
			requireFullProfileNoStoreOrRuntimeEffects(test, data, priorCPU, priorMemory)
			if fixture.storage.TransientSpoolBudgetUsage() != priorSpools {
				test.Fatal("blocked verification configured the process spool budget")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				test.Fatal("actual full root ignored cancellation while waiting to verify the schema")
			}
			if opened || !errors.Is(startupErr, context.Canceled) || !strings.HasPrefix(startupErr.Error(), schemaReadonlyPGStartupPrefix) {
				test.Fatal("actual full root did not fail closed at the verification boundary", opened, startupErr)
			}
			executionRootWait(test, func(ctx context.Context) bool {
				var sessions int
				err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend'`).Scan(&sessions)
				return err == nil && sessions == 0
			})
			fullProfileSchemaPGNoAdmission(test, observer)
			requireFullProfileNoStoreOrRuntimeEffects(test, data, priorCPU, priorMemory)
			if fixture.storage.TransientSpoolBudgetUsage() != priorSpools || fullProfileSchemaPGCatalog(test, observer) != before ||
				!reflect.DeepEqual(history, fullProfileSchemaPGHistory(test, observer, fixture.migrations)) {
				test.Fatal("canceled verification changed schema, migration history or process spool settings")
			}
		})
	}
}

func TestFullProfilePGConcurrentCurrentInfrastructureVerifiesBeforeAdmissionWithoutDDL(test *testing.T) {
	fixture := newWave48PGFixture(test)
	fullProfileSchemaPGHistory(test, fixture.observer, fixture.migrations)
	wantSchema := fullProfileSchemaPGCatalog(test, fixture.observer)
	dsn, observer := executionRootPostgresFixture(test)
	test.Setenv("CXT_POSTGRES_DSN", dsn)
	executionRootPostgresApplyMigrations(test, dsn)
	history := fullProfileSchemaPGHistory(test, observer, fixture.migrations)
	if fullProfileSchemaPGCatalog(test, observer) != wantSchema {
		test.Fatal("concurrent verification requires the same explicitly migrated current schema")
	}
	unlock := fullProfileSchemaPGLock(test, observer)
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	priorSpools := fixture.storage.TransientSpoolBudgetUsage()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var startups [3]struct {
		infrastructure *Infrastructure
		err            error
	}
	var configurations [3]InfrastructureConfig
	var names []string
	for position, role := range []outbound.ProcessRole{outbound.ProcessWorker, outbound.ProcessAPI, outbound.ProcessMCP} {
		profile, err := executionProfileFromEnvironment(role, role == outbound.ProcessWorker)
		if err != nil || profile == nil || profile.budget.Profile != outbound.BoundedWorkersProfile {
			test.Fatal("concurrent infrastructure must retain the selected full-profile source", role, err)
		}
		dataDSN, err := executionDataDSN(dsn, profile.role)
		if err != nil {
			test.Fatal(err)
		}
		names = append(names, "cxthub/"+outbound.BoundedDocumentsProfile+"/"+string(role)+"/data")
		configurations[position] = InfrastructureConfig{
			DSN: dataDSN, RequirePostgres: true, MigrationsDirectory: fixture.migrations,
		}
	}
	var workers sync.WaitGroup
	workers.Add(len(configurations))
	done := make(chan struct{})
	test.Cleanup(func() {
		cancel()
		select {
		case <-done:
			for _, startup := range startups {
				if err := startup.infrastructure.Close(); err != nil {
					test.Error("close only the owned concurrent infrastructure", err)
				}
			}
		case <-time.After(5 * time.Second):
			test.Error("concurrent infrastructure startup did not join after cancellation")
		}
	})
	for position, configuration := range configurations {
		go func() {
			defer workers.Done()
			startups[position].infrastructure, startups[position].err = OpenInfrastructure(ctx, configuration)
		}()
	}
	go func() {
		workers.Wait()
		close(done)
	}()
	fullProfileSchemaPGWaitBlocked(test, observer, names)
	fullProfileSchemaPGNoAdmission(test, observer)
	if fullProfileSchemaPGCatalog(test, observer) != wantSchema || !reflect.DeepEqual(history, fullProfileSchemaPGHistory(test, observer, fixture.migrations)) {
		test.Fatal("blocked concurrent verification changed schema or migration history")
	}
	unlock()
	select {
	case <-done:
	case <-ctx.Done():
		test.Fatal("same-source concurrent verification did not complete within its fixture lifetime", ctx.Err())
	}
	for _, startup := range startups {
		if startup.err != nil || startup.infrastructure == nil {
			test.Fatal("compatible current-source infrastructure did not finish verification", startup.err)
		}
	}
	if fullProfileSchemaPGCatalog(test, observer) != wantSchema {
		test.Fatal("concurrent verification changed the explicitly migrated current schema")
	}
	for _, startup := range startups {
		infrastructure := startup.infrastructure
		storage, postgres := infrastructure.Store.(*store.PostgresStore)
		if !postgres || infrastructure.Store != infrastructure.ownedStore || infrastructure.DocumentProfile != documentquery.VerifiedRoots || infrastructure.execution != nil {
			test.Fatal("concurrent startup lost its actual shared store or crossed process admission")
		}
		if err := storage.VerifyMigrations(ctx, fixture.migrations); err != nil {
			test.Fatal("concurrent startup accepted an unverified current schema", err)
		}
		if err := infrastructure.Close(); err != nil {
			test.Fatal(err)
		}
		if err := storage.Ping(ctx); err == nil {
			test.Fatal("concurrent infrastructure retained its owned PostgreSQL pool")
		}
	}
	if !reflect.DeepEqual(history, fullProfileSchemaPGHistory(test, observer, fixture.migrations)) || fullProfileSchemaPGCatalog(test, observer) != wantSchema {
		test.Fatal("current-source verification changed migration history, timestamps or schema")
	}
	executionRootWait(test, func(ctx context.Context) bool {
		var sessions int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend'`).Scan(&sessions)
		return err == nil && sessions == 0
	})
	fullProfileSchemaPGNoAdmission(test, observer)
	if runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory || fixture.storage.TransientSpoolBudgetUsage() != priorSpools {
		test.Fatal("before-admission verification concurrency changed process runtime or spool settings")
	}
	test.Log("same current source verified through Infrastructure before admission without DDL; no simultaneous successful roots, migration execution, mid-file interruption or old/new compatibility is established")
}
