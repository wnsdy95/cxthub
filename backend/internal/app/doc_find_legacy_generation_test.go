package app

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationCosts struct {
	opens, pages, directories, metadataBytes     int
	manifests, manifestBytes, chunks, chunkBytes int
	indexes, whole, boundaries, writes           int
	chunkLookups                                 int
}

type legacyFindGenerationFixture struct {
	generation domain.DocRangeGeneration
	binding    domain.DocReadGenerationBinding
	objects    map[domain.ContentHash][]byte
	owned      bool
	failure    error
}

type legacyFindGenerationStore struct {
	*store.FSStore
	repo     domain.ContentHash
	fixtures map[domain.ContentHash]*legacyFindGenerationFixture
	readonly bool
	prefix   int
	cost     legacyFindGenerationCosts
}

type legacyFindGenerationRead struct {
	storage *legacyFindGenerationStore
	fixture *legacyFindGenerationFixture
}

type legacyFindGenerationMeta struct {
	*store.FSStore
	snapshot domain.Snapshot
	failure  error
}

type legacyFindGenerationMalformedStore struct {
	*legacyFindGenerationStore
	mutation string
}

type legacyFindGenerationMalformedRead struct {
	outbound.DocRangeGenerationRead
	storage *legacyFindGenerationMalformedStore
}

func (storage *legacyFindGenerationMalformedStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.legacyFindGenerationStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &legacyFindGenerationMalformedRead{DocRangeGenerationRead: read, storage: storage}, nil
}

func (read *legacyFindGenerationMalformedRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	entries, err := read.DocRangeGenerationRead.ReadPage(ctx, position)
	if err != nil {
		return nil, err
	}
	switch read.storage.mutation {
	case "short page":
		return entries[:len(entries)-1], nil
	case "empty page":
		return nil, nil
	case "wrong position":
		entries[len(entries)-1].Index++
	case "read failure":
		return nil, domain.ErrIntegrity
	}
	return entries, nil
}

func (metadata *legacyFindGenerationMeta) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return metadata.snapshot, metadata.failure
}

func (storage *legacyFindGenerationStore) InReadOnlyTransaction(context.Context) bool {
	return storage.readonly
}

func (storage *legacyFindGenerationStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fixture := storage.fixtures[hash]
	if repo != storage.repo || fixture == nil || !fixture.owned {
		return nil, domain.ErrNotFound
	}
	if fixture.failure != nil {
		return nil, fixture.failure
	}
	raw, _ := json.Marshal(fixture.generation)
	storage.cost.metadataBytes += len(raw)
	return &legacyFindGenerationRead{storage: storage, fixture: fixture}, nil
}

func (read *legacyFindGenerationRead) Descriptor() domain.DocRangeGeneration {
	return read.fixture.generation
}

func (read *legacyFindGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.fixture.binding
}

func (read *legacyFindGenerationRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.fixture.generation.Envelope), nil
}

func (read *legacyFindGenerationRead) PageHash(ctx context.Context, page int) (domain.ContentHash, error) {
	return domain.ResolveDocRangeLeaf(ctx, read.fixture.generation.EventPages, page, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		raw := read.fixture.objects[hash]
		read.storage.cost.directories++
		read.storage.cost.metadataBytes += len(raw)
		return raw, nil
	})
}

func (read *legacyFindGenerationRead) ReadPage(ctx context.Context, page int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	hash, err := read.PageHash(ctx, page)
	if err != nil {
		return nil, err
	}
	raw := read.fixture.objects[hash]
	read.storage.cost.metadataBytes += len(raw)
	return read.fixture.generation.DecodePage(page, hash, raw)
}

func (*legacyFindGenerationRead) Chunk(context.Context, int) (domain.ConversationManifestChunk, error) {
	return domain.ConversationManifestChunk{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *legacyFindGenerationStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *legacyFindGenerationStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	manifest, err := storage.FSStore.GetDocManifest(ctx, repo, hash)
	raw, _ := json.Marshal(manifest)
	storage.cost.manifestBytes += len(raw)
	return manifest, err
}

func (storage *legacyFindGenerationStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.FSStore.GetChunk(ctx, repo, hash)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationStore) DocRangeBoundary(_ context.Context, repo, hash, base domain.ContentHash) (int, error) {
	storage.cost.boundaries++
	if repo != storage.repo || !storage.fixtures[hash].owned || !storage.fixtures[base].owned {
		return 0, domain.ErrNotFound
	}
	return storage.prefix, nil
}

func (storage *legacyFindGenerationStore) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.cost.writes++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}
