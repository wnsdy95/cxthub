//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ApplyMigrations applies *.sql files in dir in file name order (based on schema_migrations history for idempotency).
// Each file is applied in a transaction, and its version is recorded upon success — skipped on restart for already applied versions. Returns: number of files applied this time.
func (s *PostgresStore) ApplyMigrations(ctx context.Context, dir string) (int, error) {
	if _, err := s.pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			checksum   TEXT NOT NULL DEFAULT '',
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("schema_migrations creation: %w", err)
	}
	// Add checksum column to existing tables for compatibility with deployments before 0015. Explicitly return an error to avoid a "column does not exist" error that could cause the SELECT to fail.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`); err != nil {
		return 0, fmt.Errorf("schema_migrations checksum column addition: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // File names in 0001, 0002, … order are applied in that sequence

	applied := map[string]string{} // version → checksum
	rows, err := s.pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var v, c string
		if rows.Scan(&v, &c) == nil {
			applied[v] = c
		}
	}
	rows.Close()

	n := 0
	for _, f := range files {
		prev, isApplied := applied[f]
		sql, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			// Do not block startup if an applied file is unreadable (due to permissions or partial deployment) — just verify, do not reapply. Stop for unapplied files that need to be applied.
			if isApplied {
				continue
			}
			return n, err
		}
		sum := sha256.Sum256(sql)
		checksum := hex.EncodeToString(sum[:])
		if isApplied {
			// Already applied — if content has changed, tree and DB will be inconsistent (silently skip forbidden, fail loudly).
			if prev != "" && prev != checksum {
				return n, fmt.Errorf("%s: applied migration modified (checksum mismatch) — revert or add with new number", f)
			}
			continue
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return n, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return n, fmt.Errorf("%s application failed: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1,$2)`, f, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return n, err
		}
		if err := tx.Commit(ctx); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
