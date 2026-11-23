//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyEnvelopePGStore struct {
	*legacyFindGenerationPGStore
}

func (storage *legacyEnvelopePGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *legacyEnvelopePGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}
