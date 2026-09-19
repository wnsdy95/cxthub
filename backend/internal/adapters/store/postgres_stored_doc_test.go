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

func TestPGStoredDocProofOwnershipRollbackAndCorruption(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	doc, v := verifiedDocFixture(t)
	stop := errors.New("rollback document grant")
	err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, err := s.PutVerifiedDoc(tx, repo, v); err != nil {
			return err
		}
		if _, err := s.VerifyStoredDoc(tx, repo, doc.Hash); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof escaped rolled-back grant", err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if p, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil || !p.Matches(domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash}) {
			t.Fatal("valid proof", p, err)
		}
	}
	if _, err := s.VerifyStoredDoc(ctx, domain.HashContent([]byte("other")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-repo proof", err)
	}
	// A current ownership grant is required even after an earlier warm read.
	if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("stale grant cached", err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); err != nil {
		t.Fatal(err)
	}
	// The fixture content address is shared with other store tests. Restore its
	// physical bytes before closing the pool so corruption cannot leak to them.
	var original []byte
	if err := s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, doc.Hash).Scan(&original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.pool.Exec(context.Background(), `UPDATE blobs SET bytes=$1 WHERE hash=$2`, original, doc.Hash); err != nil {
			t.Error(err)
		}
	}()
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, []byte(`{"events":[]}`), doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("warm proof hid corruption", err)
	}
}
