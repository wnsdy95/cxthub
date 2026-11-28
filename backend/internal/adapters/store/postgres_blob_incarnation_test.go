//go:build postgres

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPGBlobIncarnationUpgradePreservesPayloadHeap(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	storage, catalog := catalogUpgradeStore(test, ctx)
	if _, err := storage.ApplyMigrations(ctx, catalog); err != nil {
		test.Fatal(err)
	}
	legacy, upgrade := test.TempDir(), test.TempDir()
	migrations := "../../../../schemas/db/migrations"
	files, err := filepath.Glob(filepath.Join(migrations, "*.sql"))
	if err != nil {
		test.Fatal(err)
	}
	const migration = "0109_blob_source_incarnation.sql"
	for _, file := range files {
		name := filepath.Base(file)
		if name <= catalogUpgradeVersion || name > migration {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			test.Fatal(err)
		}
		destination := legacy
		if name == migration {
			destination = upgrade
		}
		if err := os.WriteFile(filepath.Join(destination, name), raw, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := storage.ApplyMigrations(ctx, legacy); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) SELECT repeat(md5(ordinal::text),2),convert_to(repeat(ordinal::text,1024),'UTF8') FROM generate_series(1,1000) ordinal`)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes`)
	inspect := func() string {
		test.Helper()
		var fingerprint string
		if err := storage.pool.QueryRow(ctx, `SELECT pg_relation_filenode('blobs')::text || ':' || md5(string_agg(hash || ':' || ctid::text || ':' || xmin::text || ':' || md5(bytes) || ':' || doc_range_source_version::text,',' ORDER BY hash)) FROM blobs`).Scan(&fingerprint); err != nil {
			test.Fatal(err)
		}
		return fingerprint
	}
	before := inspect()
	raw, err := os.ReadFile(filepath.Join(upgrade, migration))
	if err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, string(raw))
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	var absent bool
	if err := storage.pool.QueryRow(ctx, `SELECT to_regclass('blob_source_incarnations') IS NULL`).Scan(&absent); err != nil || !absent {
		test.Fatal("rolled-back upgrade left metadata", err)
	}
	started := time.Now()
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
		test.Fatal("apply incarnation migration", applied, err)
	}
	test.Logf("rows=1000 metadata_upgrade_elapsed=%s payload_heap_rewritten=false", time.Since(started))
	if after := inspect(); after != before {
		test.Fatal("incarnation backfill changed bytes, source versions or heap tuples")
	}
	var blobs, incarnations, distinct int
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM blobs),count(*),count(DISTINCT incarnation) FROM blob_source_incarnations`).Scan(&blobs, &incarnations, &distinct); err != nil || blobs != incarnations || distinct != incarnations {
		test.Fatal("upgrade did not assign one distinct incarnation per object", blobs, incarnations, distinct, err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 0 {
		test.Fatal("upgrade replay was not idempotent", applied, err)
	}
}
