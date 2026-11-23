package app

import (
	"bytes"
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyCompatibilityChunkStore struct {
	*store.FSStore
	chunkReads int
	failHash   domain.ContentHash
	failure    error
}

func (storage *legacyCompatibilityChunkStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.chunkReads++
	if hash == storage.failHash && storage.failure != nil {
		return nil, storage.failure
	}
	raw, err := storage.FSStore.GetChunk(ctx, repo, hash)
	if err == nil && hash == storage.failHash {
		raw = bytes.Clone(raw)
		raw[0] ^= 1
	}
	return raw, err
}
