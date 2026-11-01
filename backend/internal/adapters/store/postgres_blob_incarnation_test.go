//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

func TestPGBlobIncarnationMetadataAndVersionCannotBeReused(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	original := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	_, err = storage.pool.Exec(ctx, `UPDATE blob_source_incarnations SET incarnation=$2::uuid WHERE hash=$1`, string(hash), original.Witness.Root.Incarnation)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		test.Fatal("incarnation update was allowed", err)
	}
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{original.Witness}, nil)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET doc_range_source_version=0 WHERE hash=$1`, string(hash))
	next := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	if next.Witness.Root.Version != original.Witness.Root.Version+1 || next.Witness.Root.Incarnation != original.Witness.Root.Incarnation {
		test.Fatal("source version assignment reset fencing")
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blob_source_incarnations WHERE hash=$1`, string(hash))
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{next.Witness}, domain.ErrIntegrity)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		object, err := storage.CaptureMemoryObject(bound, repo, hash, 2<<20)
		if !errors.Is(err, domain.ErrIntegrity) || object.Digest != nil || object.Witness.Root != nil {
			test.Fatal("missing incarnation was silently repaired or trusted", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blob_source_incarnations(hash,incarnation) VALUES($1,$2::uuid)`, string(hash), original.Witness.Root.Incarnation)
	regenerated := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	if regenerated.Witness.Root.Incarnation == original.Witness.Root.Incarnation {
		test.Fatal("explicit insertion reused previous incarnation")
	}
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{next.Witness}, domain.ErrConflict)
}
