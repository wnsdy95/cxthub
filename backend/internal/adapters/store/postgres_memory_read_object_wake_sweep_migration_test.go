//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadObjectWakeSweepMigrationPreservesPendingObligations(test *testing.T) {
	storage, ctx, _ := catalogPG(test)
	schema := pgx.Identifier{domain.NewID("cxt_memory_sweep_migration_")}.Sanitize()
	catalogExecPG(test, ctx, storage.pool, "CREATE SCHEMA "+schema)
	config := storage.pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	peer := &PostgresStore{pool: pool}
	test.Cleanup(func() {
		peer.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			test.Error(err)
		}
	})
	catalogExecPG(test, ctx, pool, `CREATE TABLE blobs(hash text PRIMARY KEY,bytes bytea NOT NULL);
 CREATE TABLE repos(id text PRIMARY KEY);
 CREATE SEQUENCE memory_read_object_wake_fence_seq AS bigint;
 CREATE TABLE memory_read_object_wakes(
 hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
 version bigint NOT NULL DEFAULT nextval('memory_read_object_wake_fence_seq') CHECK(version>0),
 after_repo text NOT NULL DEFAULT '',next_attempt timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0,reason text NOT NULL DEFAULT '')`)
	catalogExecPG(test, ctx, pool, memoryReadObjectWakeLegacyTriggerSQLPG(test))
	catalogExecPG(test, ctx, pool, `CREATE TRIGGER memory_read_wake_object AFTER UPDATE ON blobs
 FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_object_wake()`)
	delivery, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", "0122_memory_read_object_wake_delivery.sql"))
	if err != nil {
		test.Fatal(err)
	}
	setup := catalogTxPG(test, ctx, peer)
	catalogExecPG(test, ctx, setup, string(delivery))
	if err := setup.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	hash, repo := domain.HashContent([]byte("sweep migration object")), domain.HashContent([]byte("sweep migration recipient"))
	catalogExecPG(test, ctx, pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), []byte("unchanged payload"))
	catalogExecPG(test, ctx, pool, `INSERT INTO repos(id) VALUES($1)`, string(repo))
	catalogExecPG(test, ctx, pool, `INSERT INTO memory_read_object_wakes(hash,after_repo,next_attempt) VALUES($1,$2,clock_timestamp()-interval '1 hour')`, string(hash), string(repo))
	catalogExecPG(test, ctx, pool, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), int64(1), []string{string(repo)})
	job := memoryReadObjectWakeDeliveryReadPG(test, peer, ctx, hash, repo)
	var due time.Time
	var originalFunction string
	if err := pool.QueryRow(ctx, `SELECT next_attempt,pg_get_functiondef('cxt_memory_read_object_wake()'::regprocedure)
 FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).Scan(&due, &originalFunction); err != nil {
		test.Fatal(err)
	}
	const filename = "0123_memory_read_object_wake_sweep.sql"
	raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", filename))
	if err != nil {
		test.Fatal(err)
	}
	upgrade := test.TempDir()
	if err := os.WriteFile(filepath.Join(upgrade, filename), raw, 0600); err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, peer)
	catalogExecPG(test, ctx, transaction, string(raw))
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	var exists bool
	var currentFunction string
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute
 WHERE attrelid='memory_read_object_wakes'::regclass AND attname IN ('scan_version','through_repo') AND NOT attisdropped),
 pg_get_functiondef('cxt_memory_read_object_wake()'::regprocedure)`).Scan(&exists, &currentFunction); err != nil || exists || currentFunction != originalFunction {
		test.Fatal("migration rollback left a column or changed the trigger", exists, err)
	}
	blocker := catalogTxPG(test, ctx, peer)
	catalogExecPG(test, ctx, blocker, `LOCK TABLE memory_read_object_wakes IN ACCESS SHARE MODE`)
	started := time.Now()
	applied, err := peer.ApplyMigrations(ctx, upgrade)
	var lockError *pgconn.PgError
	if applied != 0 || !errors.As(err, &lockError) || lockError.Code != "55P03" || time.Since(started) > 2*time.Second {
		test.Fatal("sweep migration did not bound its metadata lock wait", applied, time.Since(started), err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if applied, err := peer.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
		test.Fatal("apply sweep migration", applied, err)
	}
	if applied, err := peer.ApplyMigrations(ctx, upgrade); err != nil || applied != 0 {
		test.Fatal("sweep migration replay was not idempotent", applied, err)
	}
	legacy := memoryReadObjectWakeReadPG(test, peer, ctx, hash)
	if legacy.scanVersion != 0 || legacy.throughRepo.Valid || legacy.version != 1 || legacy.afterRepo != string(repo) || !legacy.nextAttempt.Equal(due) {
		test.Fatal("metadata migration rewrote the pending legacy cursor, fence or due age", legacy)
	}
	catalogExecPG(test, ctx, pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	first := memoryReadObjectWakeReadPG(test, peer, ctx, hash)
	catalogExecPG(test, ctx, pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	second := memoryReadObjectWakeReadPG(test, peer, ctx, hash)
	if first.scanVersion != legacy.version || second.scanVersion != legacy.version || second.version <= first.version || first.version <= legacy.version || second.afterRepo != legacy.afterRepo || second.nextAttempt != legacy.nextAttempt {
		test.Fatal("first post-migration write failed to preserve the older cursor's sweep fence", legacy, first, second)
	}
	writer := catalogTxPG(test, ctx, peer)
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if err := writer.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadObjectWakeReadPG(test, peer, ctx, hash); actual != second {
		test.Fatal("rolled-back physical update changed the sweep obligation", second, actual)
	}
	if actual := memoryReadObjectWakeDeliveryReadPG(test, peer, ctx, hash, repo); actual != job {
		test.Fatal("migration or writer silently rewrote an existing delivery", job, actual)
	}
	for _, invalid := range []int64{-1, second.version + 1} {
		_, err := pool.Exec(ctx, `UPDATE memory_read_object_wakes SET scan_version=$2 WHERE hash=$1`, string(hash), invalid)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			test.Fatal("invalid sweep fence accepted", invalid, err)
		}
	}
	var body []byte
	if err := pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&body); err != nil || string(body) != "unchanged payload" {
		test.Fatal("sweep migration rewrote object payload", err)
	}
	test.Log("0123 adds default-zero version and lazy upper owner fences without recipient backfill; pending legacy cursors and deliveries survive, and the next write freezes the pre-update version")
}
