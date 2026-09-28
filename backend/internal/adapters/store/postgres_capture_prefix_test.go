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

func TestPGCaptureComparisonRechecksOwnershipAndRollback(t *testing.T) {
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
	if _, err = s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	old, next := checkCaptureComparison(t, s, repo)
	if _, err = s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, next.Hash); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("stale grant", got, err)
	}
	stop := errors.New("rollback grant after proof")
	err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, err := s.PutDoc(tx, repo, next); err != nil {
			return err
		}
		got, err := s.CaptureSupersedes(tx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native")
		if err != nil {
			return err
		}
		if !got {
			t.Fatal("missing containment")
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rolled-back proof", got, err)
	}
	if _, err = s.PutDoc(ctx, repo, next); err != nil {
		t.Fatal(err)
	}
	var original []byte
	if err = s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, next.Hash).Scan(&original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.pool.Exec(context.Background(), `UPDATE blobs SET bytes=$2 WHERE hash=$1`, next.Hash, original); err != nil {
			t.Error(err)
		}
	}()
	if _, err = s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, next.Hash, []byte(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("warm proof hid corruption", got, err)
	}
}
