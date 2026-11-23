//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type visibleCostStore struct {
	*docViewPGStore
	unitReads, chunkReads, chunkBytes int
}

func (storage *visibleCostStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	storage.chunkBytes += int(length)
	return storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}
