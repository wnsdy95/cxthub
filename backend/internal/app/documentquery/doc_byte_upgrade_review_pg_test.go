//go:build postgres

package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type snapshotlessLegacyBasePGCosts struct {
	opens, pages, chunkLookups, manifests, chunks, whole, indexes int
}

type snapshotlessLegacyBasePGStore struct {
	*store.PostgresStore
	cost snapshotlessLegacyBasePGCosts
}

type snapshotlessLegacyBasePGRead struct {
	outbound.DocRangeGenerationRead
	storage *snapshotlessLegacyBasePGStore
}

func (storage *snapshotlessLegacyBasePGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &snapshotlessLegacyBasePGRead{DocRangeGenerationRead: read, storage: storage}, nil
}

func (read *snapshotlessLegacyBasePGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (read *snapshotlessLegacyBasePGRead) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	read.storage.cost.chunkLookups++
	return read.DocRangeGenerationRead.Chunk(ctx, position)
}

func (storage *snapshotlessLegacyBasePGStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}
