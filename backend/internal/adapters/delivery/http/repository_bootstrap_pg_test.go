//go:build postgres

package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type bootstrapAuthorizationPGStore struct {
	*repositoryStateAuthorizationPGStore
	points int
}

func (storage *bootstrapAuthorizationPGStore) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	storage.points++
	return storage.PostgresStore.GetSnapshot(ctx, repo, id)
}
