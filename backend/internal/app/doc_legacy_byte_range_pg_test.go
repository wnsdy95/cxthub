//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyByteCompatibilityPGStore struct {
	*legacyFindGenerationPGStore
}

func (storage *legacyByteCompatibilityPGStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return storage.PostgresStore.GetDoc(ctx, repo, hash)
}
