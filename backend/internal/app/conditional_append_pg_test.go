//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type conditionalAppendRevisionFailure struct{ *store.PostgresStore }

func (storage conditionalAppendRevisionFailure) AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error {
	return domain.ErrIntegrity
}

type conditionalAppendEnteringStore struct {
	*store.PostgresStore
	entered chan struct{}
}

func (storage conditionalAppendEnteringStore) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	close(storage.entered)
	return storage.PostgresStore.WithinRepository(ctx, repo, fn)
}
