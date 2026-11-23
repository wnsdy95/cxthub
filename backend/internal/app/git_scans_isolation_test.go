package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type headClaimIsolationStore struct {
	*store.FSStore
	repos       []domain.Repo
	claims      []domain.ContentHash
	failures    map[domain.ContentHash]error
	beforeClaim func(domain.ContentHash)
}

func (storage *headClaimIsolationStore) ListRepos(context.Context, string) ([]domain.Repo, error) {
	return append([]domain.Repo{}, storage.repos...), nil
}
