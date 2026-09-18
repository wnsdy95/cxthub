//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"testing"
	"time"
)

func TestPGRepositoryRevisionAtomicAndShared(t *testing.T) {
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
	peer, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	repo := domain.HashContent([]byte(fmt.Sprint(t.Name(), time.Now().UnixNano())))
	if _, err = s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort")
	if err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := s.AdvanceRepositoryRevision(tx, repo, true); err != nil {
			return err
		}
		v, e := peer.RepositoryRevision(ctx, repo)
		if e != nil || v.Pending != 0 {
			t.Fatalf("uncommitted revision visible: %+v %v", v, e)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if v, err := peer.RepositoryRevision(ctx, repo); err != nil || v.Pending != 0 {
		t.Fatalf("rollback: %+v %v", v, err)
	}
	if err = s.WithinRepository(ctx, repo, func(tx context.Context) error { return s.AdvanceRepositoryRevision(tx, repo, true) }); err != nil {
		t.Fatal(err)
	}
	if v, err := peer.RepositoryRevision(ctx, repo); err != nil || v.Pending != 1 || v.Graph != 0 {
		t.Fatalf("peer: %+v %v", v, err)
	}
}
