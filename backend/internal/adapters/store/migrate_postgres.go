//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ApplyMigrations applies *.sql files in dir in file name order (based on schema_migrations history for idempotency).
// Each file is applied in a transaction, and its version is recorded upon success — skipped on restart for already applied versions. Returns: number of files applied this time.
func (s *PostgresStore) ApplyMigrations(ctx context.Context, dir string) (int, error) {
	return s.applyMigrations(ctx, dir, false)
}

func (storage *PostgresStore) ApplyPublicMigrations(ctx context.Context, directory string) (int, error) {
	return storage.applyMigrations(ctx, directory, true)
}

func (storage *PostgresStore) applyMigrations(ctx context.Context, dir string, public bool) (int, error) {
	manifest, err := migrationManifest(ctx, dir)
	if err != nil {
		return 0, err
	}
	if public {
		if err := verifyMigrationCatalog(manifest); err != nil {
			return 0, err
		}
	}
	conn, err := storage.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		return 0, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	if public {
		if err := verifyMigrationNamespace(ctx, conn); err != nil {
			return 0, err
		}
	}
	var historyExists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&historyExists); err != nil {
		return 0, err
	}
	if public && historyExists {
		var safe bool
		if err := conn.QueryRow(ctx, `SELECT COALESCE((SELECT relkind='r' AND NOT relrowsecurity AND NOT relforcerowsecurity
 FROM pg_class WHERE oid=to_regclass('public.schema_migrations')),false)`).Scan(&safe); err != nil {
			return 0, err
		}
		if !safe {
			return 0, fmt.Errorf("%w: migration history table is unsafe", ErrSchemaMismatch)
		}
		previous, err := readMigrationHistory(ctx, conn, "public.schema_migrations")
		if err != nil {
			return 0, err
		}
		if err := migrationHistoryMatches(manifest, previous, false); err != nil {
			return 0, err
		}
	}
	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			checksum   TEXT NOT NULL DEFAULT '',
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("schema_migrations creation: %w", err)
	}
	// Add checksum column to existing tables for compatibility with deployments before 0015. Explicitly return an error to avoid a "column does not exist" error that could cause the SELECT to fail.
	if _, err := conn.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`); err != nil {
		return 0, fmt.Errorf("schema_migrations checksum column addition: %w", err)
	}
	applied, err := readMigrationHistory(ctx, conn, "schema_migrations")
	if err != nil {
		return 0, err
	}
	if err := migrationHistoryMatches(manifest, migrationApplyHistory(manifest, applied, public), false); err != nil {
		return 0, err
	}

	n := 0
	for _, migration := range manifest {
		if _, isApplied := applied[migration.name]; isApplied {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(dir, migration.name))
		if err != nil {
			return n, err
		}
		sum := sha256.Sum256(sql)
		checksum := hex.EncodeToString(sum[:])
		if checksum != migration.checksum {
			return n, fmt.Errorf("%w: migration %q changed after preflight", ErrSchemaMismatch, migration.name)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return n, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return n, fmt.Errorf("%s application failed: %w", migration.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1,$2)`, migration.name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return n, err
		}
		if err := tx.Commit(ctx); err != nil {
			return n, err
		}
		n++
	}
	if _, present := applied["0052_repository_organizations.sql"]; present || manifestContains(manifest, "0052_repository_organizations.sql") {
		if err := storage.migrateRepositoryOwnership(ctx, conn); err != nil {
			return n, err
		}
	}
	return n, nil
}

func containsMigration(files []string, name string) bool {
	for _, f := range files {
		if f == name {
			return true
		}
	}
	return false
}
