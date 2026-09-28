//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Service.Send uses a repeatable-read, read-only transaction. The production
// manifest path must work inside that exact boundary, including existing docs.
func TestPGManifestInReadSnapshot(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	doc, verified := verifiedDocFixture(t)
	if _, err = st.PutVerifiedDoc(ctx, repo, verified); err != nil {
		t.Fatal(err)
	}
	err = st.WithinReadSnapshot(ctx, func(read context.Context) error {
		_, err := st.GetDocManifest(read, repo, doc.Hash)
		return err
	})
	if err != nil {
		t.Fatalf("production pull manifest: %v", err)
	}
	// A publisher can hold a row lock without blocking an already committed
	// manifest read. This must not rely on a raised HTTP timeout.
	locked, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(locked)
	if _, err = locked.Exec(ctx, `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`, doc.Hash); err != nil {
		t.Fatal(err)
	}
	read, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = st.WithinReadSnapshot(read, func(read context.Context) error {
		_, err := st.GetDocManifest(read, repo, doc.Hash)
		return err
	}); err != nil {
		t.Fatalf("read blocked by publication lock: %v", err)
	}
	if _, err = st.GetDocManifest(ctx, domain.HashContent([]byte("foreign")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign manifest: %v", err)
	}
	if err = locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Legacy bodies are returned via the existing full-body fallback. A read
	// snapshot must never acquire repacking writes or mutate global blobs.
	if _, err = st.pool.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, docCompress(verified.Bytes()), doc.Hash); err != nil {
		t.Fatal(err)
	}
	if err = st.WithinReadSnapshot(ctx, func(read context.Context) error {
		_, err := st.GetDocManifest(read, repo, doc.Hash)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("legacy manifest fallback: %v", err)
		}
		got, err := st.GetDoc(read, repo, doc.Hash)
		if err == nil {
			err = domain.ValidateSessionDocHash(got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
