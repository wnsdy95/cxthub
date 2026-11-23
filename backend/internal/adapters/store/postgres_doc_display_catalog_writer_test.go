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

func docCatalogWriterCodePG(test *testing.T, err error, expected string) {
	test.Helper()
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != expected {
		test.Fatal("writer fence SQLSTATE", err, expected)
	}
}

func TestDocDisplayCatalogWriterPGBusyMigrationRollback(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := rootProjectionIsolatedDSN(test, ctx)
	storage, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	directory := test.TempDir()
	entries, err := os.ReadDir("../../../../schemas/db/migrations")
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" || entry.Name() > "0137_doc_display_catalog_census.sql" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), raw, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := storage.ApplyMigrations(ctx, directory); err != nil {
		test.Fatal(err)
	}
	repo, _ := rootPGRepo(test, ctx, storage)
	seed, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(seed)
	if _, err := seed.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='5'`); err != nil {
		test.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,event_index,event_hash,profile,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,version,lease_until,worker_protocol,created_at,next_attempt)
 VALUES($1,$2,0,$3,'public-event-display-v1',0,100,0,'',gen_random_uuid(),0,$2,
 'running',7,clock_timestamp()+interval '1 minute',5,clock_timestamp(),clock_timestamp())`,
		repo, domain.HashContent([]byte("busy migration doc")), domain.HashContent([]byte("busy migration event"))); err != nil {
		test.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	holder, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(holder)
	if _, err := holder.Exec(ctx, `SELECT repo_id FROM doc_display_jobs_v1 WHERE repo_id=$1 FOR UPDATE`, repo); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0138_doc_display_catalog_writer.sql")
	if err != nil {
		test.Fatal(err)
	}
	upgrade, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(upgrade)
	_, err = upgrade.Exec(ctx, string(raw))
	docCatalogWriterCodePG(test, err, "55P03")
	rollbackPG(upgrade)
	var unchanged bool
	if err := storage.pool.QueryRow(ctx, `SELECT state='running' AND version=7 AND lease_until IS NOT NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_writer(text,text,integer,text,bigint)') IS NULL
 FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo).Scan(&unchanged); err != nil || !unchanged {
		test.Fatal("busy migration partially changed claim/schema", unchanged, err)
	}
	rollbackPG(holder)
	upgrade, err = storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(upgrade)
	if _, err := upgrade.Exec(ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	if err := upgrade.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT state='waiting' AND version=8 AND lease_until IS NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_writer(text,text,integer,text,bigint)') IS NOT NULL
 FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo).Scan(&unchanged); err != nil || !unchanged {
		test.Fatal("migration retry did not atomically drain and fence", unchanged, err)
	}
}
