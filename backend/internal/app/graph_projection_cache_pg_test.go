//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphCachePGRootPolicy struct{ *store.PostgresStore }

func (storage graphCachePGRootPolicy) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	metadata, err := storage.PostgresStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	return metadata, err
}
