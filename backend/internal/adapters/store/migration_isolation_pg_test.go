//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGVerifyMigrationsSeesCommittedOwnerAfterWaitingUnderSessionIsolation(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal(err)
	}
	for _, isolation := range []string{"repeatable read", "serializable"} {
		test.Run(isolation, func(test *testing.T) {
			configuration := fixture.config.Copy()
			configuration.ConnConfig.RuntimeParams["search_path"] = "public"
			configuration.ConnConfig.RuntimeParams["default_transaction_isolation"] = isolation
			pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
			if err != nil {
				test.Fatal(err)
			}
			defer pool.Close()
			reader := &PostgresStore{pool: pool}
			owner, err := storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(owner)
			if _, err := owner.Exec(fixture.ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
				test.Fatal(err)
			}
			if _, err := owner.Exec(fixture.ctx, `INSERT INTO public.schema_migrations(version,checksum) VALUES('9999_post_lock_unknown.sql',repeat('0',64))`); err != nil {
				test.Fatal(err)
			}
			work, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				completed <- reader.VerifyMigrations(work, migrationVerificationBundlePG)
			}()
			test.Cleanup(func() {
				cancel()
				select {
				case <-joined:
				case <-time.After(5 * time.Second):
					test.Error("schema verification did not join after cancellation")
				}
			})
			for {
				var waiting bool
				if err := fixture.observer.QueryRow(work, `SELECT EXISTS(SELECT 1 FROM pg_locks held
 JOIN pg_locks blocked USING(locktype,database,classid,objid,objsubid)
 WHERE held.pid=$1 AND held.locktype='advisory' AND held.granted AND held.mode='ExclusiveLock'
 AND NOT blocked.granted AND blocked.mode='ShareLock')`, owner.Conn().PgConn().PID()).Scan(&waiting); err != nil {
					test.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-completed:
					test.Fatal("verification returned before the real migration lock was released", err)
				case <-work.Done():
					test.Fatal("verification did not wait on the real owner", work.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := owner.Commit(work); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-completed:
				if !errors.Is(err, ErrSchemaMismatch) {
					test.Fatal("verification read a pre-migration snapshot after acquiring the shared lock", isolation, err)
				}
			case <-work.Done():
				test.Fatal("verification failed to observe the committed owner", work.Err())
			}
			if pool.Stat().AcquiredConns() != 0 {
				test.Fatal("schema mismatch retained its read transaction")
			}
			if _, err := storage.pool.Exec(fixture.ctx, `DELETE FROM public.schema_migrations WHERE version='9999_post_lock_unknown.sql'`); err != nil {
				test.Fatal(err)
			}
			if err := reader.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
				test.Fatal("restoring the current source did not restore verification", err)
			}
			var current string
			if err := pool.QueryRow(fixture.ctx, `SHOW default_transaction_isolation`).Scan(&current); err != nil || current != isolation {
				test.Fatal("per-verification isolation override changed the session default", current, err)
			}
		})
	}
}
