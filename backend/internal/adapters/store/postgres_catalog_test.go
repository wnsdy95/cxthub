//go:build postgres

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Helpers for tests that mix public adapter calls and direct SQL on a
// repository of their own.
func catalogPG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash) {
	t.Helper()
	s, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, repo
}

type catalogPGExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func catalogExecPG(t *testing.T, ctx context.Context, db catalogPGExecutor, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("catalog fixture SQL: %v", err)
	}
}

func catalogTxPG(t *testing.T, ctx context.Context, s *PostgresStore) pgx.Tx {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}
