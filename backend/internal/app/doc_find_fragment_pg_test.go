//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docFindFragmentPGStore struct {
	*docVisibleLegacyBasePGStore
	chunkReads int
	chunkBytes int
}

func (storage *docFindFragmentPGStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	storage.chunkBytes += int(length)
	return storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (storage *docFindFragmentPGStore) ReadVerifiedDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	storage.test.Error("fragment read materialized a complete verified Root")
	return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
}

type docFindLegacyFragmentPGStore struct {
	*store.PostgresStore
	wholeReads int
	chunkReads int
	chunkBytes int
	chunks     []domain.ContentHash
}

func (storage *docFindLegacyFragmentPGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.wholeReads++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *docFindLegacyFragmentPGStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.chunkReads++
	storage.chunks = append(storage.chunks, hash)
	raw, err := storage.PostgresStore.GetChunk(ctx, repo, hash)
	storage.chunkBytes += len(raw)
	return raw, err
}
