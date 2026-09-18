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

func TestPGVerifiedDocTransactionAndIntegrity(t *testing.T) {
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
	doc, v := verifiedDocFixture(t)
	if _, err := s.PutVerifiedDoc(ctx, repo, domain.VerifiedSessionDoc{}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("zero proof accepted", err)
	}
	stop := errors.New("rollback verified document")
	err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, err := s.PutVerifiedDoc(tx, repo, v); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, err := s.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("body/ownership escaped rollback", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_indexes_v2 WHERE hash=$1`, doc.Hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("index escaped rollback", count, err)
	}
	if fresh, err := s.PutVerifiedDoc(ctx, repo, v); err != nil || !fresh {
		t.Fatal("first put", fresh, err)
	}
	if fresh, err := s.PutVerifiedDoc(ctx, repo, v); err != nil || fresh {
		t.Fatal("replay", fresh, err)
	}
	// An old replica's index stays isolated while this replica rebuilds its
	// own canonical-event projection lazily from the immutable document.
	if _, err := s.pool.Exec(ctx, `INSERT INTO doc_read_indexes(hash,version,envelope,event_count) VALUES($1,1,'{}',999)`, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM doc_read_indexes_v2 WHERE hash=$1`, doc.Hash); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchDocEvents(ctx, repo, doc.Hash, "earlier", -1, 10)
	if err != nil || len(hits) != 1 || hits[0].Index != 0 || hits[0].Seq != 1 || hits[0].Role != "user" {
		t.Fatalf("search metadata: %+v %v", hits, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT event_count FROM doc_read_indexes WHERE hash=$1`, doc.Hash).Scan(&count); err != nil || count != 999 {
		t.Fatal("modified old replica index", count, err)
	}
	if _, err := s.GetDoc(ctx, domain.HashContent([]byte("foreign")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign read", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, doc.Hash, []byte(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corrupt stored doc accepted", err)
	}
}
