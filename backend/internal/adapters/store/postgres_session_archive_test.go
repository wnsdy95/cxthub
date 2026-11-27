//go:build postgres

package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func sessionArchivePG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, repo
}
