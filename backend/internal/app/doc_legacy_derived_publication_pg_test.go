//go:build postgres

package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyDerivedPublicationPGStore struct {
	*legacyFindGenerationPGStore
	test         *testing.T
	repo         domain.ContentHash
	hash         domain.ContentHash
	ordinals     []int
	derivedBytes int
	failure      error
}

var _ outbound.LegacyDerivedByteRangeReader = (*legacyDerivedPublicationPGStore)(nil)

func (storage *legacyDerivedPublicationPGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) ReadLegacyDerivedByteChunk(ctx context.Context, repo, hash domain.ContentHash, binding domain.DocReadGenerationBinding, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	storage.test.Helper()
	if repo != storage.repo || hash != storage.hash || !outbound.DocReadOnly(ctx) || !storage.InReadOnlyTransaction(ctx) ||
		binding.Validate() != nil || binding != storage.bindings[hash] || descriptor.Bytes <= 0 || descriptor.Bytes > domain.ConversationManifestChunkBytes {
		storage.test.Fatal("derived read lost its selected source, generation binding, snapshot or byte bound", repo, hash, binding, ordinal, descriptor)
	}
	storage.ordinals = append(storage.ordinals, ordinal)
	if ordinal == 1 && storage.failure != nil {
		return nil, storage.failure
	}
	body, err := storage.PostgresStore.ReadLegacyDerivedByteChunk(ctx, repo, hash, binding, ordinal, descriptor)
	if err == nil && (int64(len(body)) != descriptor.Bytes || domain.HashContent(body) != descriptor.Hash) {
		storage.test.Fatal("PostgreSQL returned bytes outside the selected derived descriptor", ordinal, descriptor, len(body))
	}
	storage.derivedBytes += len(body)
	return body, err
}

func (storage *legacyDerivedPublicationPGStore) resetReads() {
	storage.cost = legacyFindGenerationCosts{}
	storage.ordinals = nil
	storage.derivedBytes = 0
}
