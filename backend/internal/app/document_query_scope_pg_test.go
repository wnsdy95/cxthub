//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGDocumentEventQueriesRejectNormalAuthenticationInsideSnapshot(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx := systemTestContext()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	repository, err := storage.PutRepo(ctx, domain.Repo{ID: hh(test.Name()), DefaultBranch: "main"})
	if err != nil {
		test.Fatal(err)
	}
	service := NewService(storage, storage, nil, nil, nil)
	queries, err := service.DocumentEventQueriesForProfile(documentquery.VerifiedRoots)
	if err != nil {
		test.Fatal(err)
	}
	callbacks := 0
	ctx = inbound.WithRepositoryActor(ctx, "reader")
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		callbacks++
		_, err := storage.PutRepo(bound, repository)
		return bound, err
	})
	err = storage.WithinReadSnapshot(ctx, func(pinned context.Context) error {
		hash := hh("unread document")
		operations := []func() error{
			func() error { _, err := queries.ReadDocEvents(pinned, repository.ID, hash, "", 0, 1); return err },
			func() error { _, err := queries.ReadDocFragments(pinned, repository.ID, hash, 0, 0, 1, 64); return err },
			func() error {
				_, err := queries.SearchDocEvents(pinned, repository.ID, hash, "needle", -1, 1)
				return err
			},
		}
		for _, operation := range operations {
			if err := operation(); !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Error("query entered general authentication from an existing read-only snapshot", err)
			}
		}
		if callbacks != 0 {
			test.Error("query invoked a write-capable authentication callback", callbacks)
		}
		if _, err := storage.GetRepo(pinned, repository.ID); err != nil {
			test.Error("query poisoned the caller's read-only transaction", err)
			return err
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
}
