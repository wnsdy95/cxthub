//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingNoticeMutationStore struct {
	*store.PostgresStore
	afterRevision func()
}

func (storage *pendingNoticeMutationStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	revision, err := storage.PostgresStore.RepositoryRevision(ctx, repo)
	if mutate := storage.afterRevision; err == nil && mutate != nil {
		storage.afterRevision = nil
		mutate()
	}
	return revision, err
}
