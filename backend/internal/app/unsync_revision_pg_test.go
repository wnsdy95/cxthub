//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func newUnsyncRevisionPGService(t *testing.T) (*Service, *store.PostgresStore) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	return NewService(storage, storage, nil, gitengine.NewEngine(storage), storage), storage
}

func TestPGUnsyncDeletionRevisions(t *testing.T) {
	service, storage := newUnsyncRevisionPGService(t)
	exerciseUnsyncDeletionRevisions(t, service, storage)
}

func TestPGUnsyncDeletionRollback(t *testing.T) {
	for _, mode := range []string{"delete", "already-shared"} {
		t.Run(mode, func(t *testing.T) {
			service, storage := newUnsyncRevisionPGService(t)
			ctx := systemTestContext()
			repo, target, descendant := prepareUnsyncRevisionFixture(t, storage)
			if err := service.PutUnsync(ctx, repo, "alice", "main", domain.Unsync{Target: target}); err != nil {
				t.Fatal(err)
			}
			if mode == "already-shared" {
				if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: descendant}, ""); err != nil {
					t.Fatal(err)
				}
			}
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback unsync deletion")
			err = repositoryWriteError(context.WithValue(ctx, revisionScopeKey{}, "none"), service, repo, func(bound context.Context) error {
				var err error
				if mode == "delete" {
					err = service.DeleteUnsync(bound, repo, "alice", "main")
				} else {
					err = service.PutUnsync(bound, repo, "alice", "main", domain.Unsync{Target: target})
				}
				if err != nil {
					return err
				}
				want := before
				want.Graph++
				assertUnsyncRevision(t, bound, storage, repo, want)
				pointers, err := storage.ListUnsyncs(bound, repo)
				if err != nil || len(pointers) != 0 {
					t.Fatalf("deletion not visible in transaction: %+v %v", pointers, err)
				}
				assertUnsyncRevision(t, ctx, storage, repo, before)
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("rollback error=%v", err)
			}
			assertUnsyncRevision(t, ctx, storage, repo, before)
			pointers, err := storage.ListUnsyncs(ctx, repo)
			if err != nil || len(pointers) != 1 || pointers[0].Target != target {
				t.Fatalf("deletion escaped rollback: %+v %v", pointers, err)
			}
		})
	}
}
