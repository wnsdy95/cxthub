//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type associationCatalogPG struct {
	*store.PostgresStore
	limit          int
	rejectSave     bool
	resetRead      bool
	originOverride string
	duringRead     func(context.Context)
}

func (storage *associationCatalogPG) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	repo, err := storage.PostgresStore.GetRepo(ctx, id)
	if err == nil && storage.originOverride != "" {
		repo.GitRemoteURL = storage.originOverride
	}
	return repo, err
}
