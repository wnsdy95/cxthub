//go:build postgres

package app

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationPGStore struct {
	*store.PostgresStore
	cost     legacyFindGenerationCosts
	pages    map[domain.ContentHash]int
	bindings map[domain.ContentHash]domain.DocReadGenerationBinding
}

type legacyFindGenerationPGRead struct {
	outbound.DocRangeGenerationRead
	storage *legacyFindGenerationPGStore
	hash    domain.ContentHash
}

func (storage *legacyFindGenerationPGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	raw, _ := read.Descriptor().Encode()
	storage.cost.metadataBytes += len(raw)
	storage.bindings[hash] = read.Binding()
	return &legacyFindGenerationPGRead{DocRangeGenerationRead: read, storage: storage, hash: hash}, nil
}

func (read *legacyFindGenerationPGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	read.storage.pages[read.hash]++
	entries, err := read.DocRangeGenerationRead.ReadPage(ctx, position)
	raw, _ := json.Marshal(entries)
	read.storage.cost.metadataBytes += len(raw)
	return entries, err
}

func (read *legacyFindGenerationPGRead) PageHash(ctx context.Context, position int) (domain.ContentHash, error) {
	read.storage.cost.directories++
	return read.DocRangeGenerationRead.PageHash(ctx, position)
}

func (read *legacyFindGenerationPGRead) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	read.storage.cost.chunkLookups++
	chunk, err := read.DocRangeGenerationRead.Chunk(ctx, position)
	raw, _ := json.Marshal(chunk)
	read.storage.cost.metadataBytes += len(raw)
	return chunk, err
}

func (storage *legacyFindGenerationPGStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationPGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationPGStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	manifest, err := storage.PostgresStore.GetDocManifest(ctx, repo, hash)
	raw, _ := json.Marshal(manifest)
	storage.cost.manifestBytes += len(raw)
	return manifest, err
}

func (storage *legacyFindGenerationPGStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.PostgresStore.GetChunk(ctx, repo, hash)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationPGStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationPGStore) DocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash) (int, error) {
	storage.cost.boundaries++
	return storage.PostgresStore.DocRangeBoundary(ctx, repo, hash, base)
}
