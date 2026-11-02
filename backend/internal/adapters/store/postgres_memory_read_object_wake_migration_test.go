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

func TestPGMemoryReadObjectWakeDeliveryMigration(test *testing.T) {
	storage, ctx, _ := catalogPG(test)
	schema := pgx.Identifier{domain.NewID("cxt_memory_fanout_migration_")}.Sanitize()
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
 CREATE TABLE memory_read_object_wakes(hash text PRIMARY KEY,version bigint NOT NULL,after_repo text NOT NULL)`)
	hash, repo := domain.HashContent([]byte("migration object")), domain.HashContent([]byte("migration recipient"))
	catalogExecPG(test, ctx, pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), []byte("unchanged payload"))
	catalogExecPG(test, ctx, pool, `INSERT INTO repos(id) VALUES($1)`, string(repo))
	catalogExecPG(test, ctx, pool, `INSERT INTO memory_read_object_wakes(hash,version,after_repo) VALUES($1,17,$2)`, string(hash), string(repo))
	const filename = "0122_memory_read_object_wake_delivery.sql"
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
	var absent bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('memory_read_object_wake_deliveries') IS NULL`).Scan(&absent); err != nil || !absent {
		test.Fatal("rolled-back migration left a delivery table", absent, err)
	}
	blocker := catalogTxPG(test, ctx, peer)
	catalogExecPG(test, ctx, blocker, `LOCK TABLE blobs IN ACCESS EXCLUSIVE MODE`)
	started := time.Now()
	applied, err := peer.ApplyMigrations(ctx, upgrade)
	var lockError *pgconn.PgError
	if applied != 0 || !errors.As(err, &lockError) || lockError.Code != "55P03" || time.Since(started) > 2*time.Second {
		test.Fatal("migration did not bound its foreign-key table lock wait", applied, time.Since(started), err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if applied, err := peer.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
		test.Fatal("apply delivery migration", applied, err)
	}
	if applied, err := peer.ApplyMigrations(ctx, upgrade); err != nil || applied != 0 {
		test.Fatal("migration replay was not idempotent", applied, err)
	}
	var version int64
	var afterRepo string
	var body []byte
	if err := pool.QueryRow(ctx, `SELECT wake.version,wake.after_repo,blob.bytes FROM memory_read_object_wakes wake JOIN blobs blob USING(hash) WHERE hash=$1`, string(hash)).Scan(&version, &afterRepo, &body); err != nil || version != 17 || afterRepo != string(repo) || string(body) != "unchanged payload" {
		test.Fatal("migration rewrote pending fanout or payload", version, afterRepo, err)
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, peer, ctx, hash); count != 0 {
		test.Fatal("migration created unbounded recipient backfill", count)
	}
	_, err = pool.Exec(ctx, `INSERT INTO memory_read_object_wake_deliveries(hash,repo_id,version) VALUES($1,$2,0)`, string(hash), string(repo))
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		test.Fatal("migration accepted an unfenced recipient job", err)
	}
	catalogExecPG(test, ctx, pool, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), int64(17), []string{string(repo)})
	catalogExecPG(test, ctx, pool, `DELETE FROM memory_read_object_wakes WHERE hash=$1`, string(hash))
	if count := memoryReadObjectWakeDeliveryCountPG(test, peer, ctx, hash); count != 1 {
		test.Fatal("parent acknowledgement cascaded durable recipient obligations", count)
	}
	catalogExecPG(test, ctx, pool, `DELETE FROM repos WHERE id=$1`, string(repo))
	if count := memoryReadObjectWakeDeliveryCountPG(test, peer, ctx, hash); count != 0 {
		test.Fatal("repository deletion did not retire obsolete obligations", count)
	}
	catalogExecPG(test, ctx, pool, `INSERT INTO repos(id) VALUES($1)`, string(repo))
	catalogExecPG(test, ctx, pool, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), int64(18), []string{string(repo)})
	catalogExecPG(test, ctx, pool, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	if count := memoryReadObjectWakeDeliveryCountPG(test, peer, ctx, hash); count != 0 {
		test.Fatal("object deletion did not retire obsolete obligations", count)
	}
}
