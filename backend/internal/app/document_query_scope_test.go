package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentQueryScopeStore struct {
	*graphReadTransactions
	repositories atomic.Int64
	snapshots    atomic.Int64
	payloads     atomic.Int64
	required     domain.DocumentIdentity
	payloadError error
	finishError  error
	observe      func(context.Context) error
}

func (storage *documentQueryScopeStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	gate, _ := ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	if permit == nil || permit.owner != gate || !permit.live.Load() || permit.class != ResourceBulk {
		return errors.New("document query read escaped its live shared bulk permit")
	}
	if !storage.InTransaction(ctx) || storage.active.Load() != 1 {
		return errors.New("document query read escaped its pinned repository snapshot")
	}
	if storage.observe != nil {
		return storage.observe(ctx)
	}
	return nil
}

func (storage *documentQueryScopeStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	nested := storage.InTransaction(ctx)
	err := storage.graphReadTransactions.WithinReadSnapshot(ctx, read)
	if err == nil && !nested {
		return storage.finishError
	}
	return err
}

func (storage *documentQueryScopeStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.repositories.Add(1)
	if err := storage.check(ctx); err != nil {
		return domain.Repo{}, err
	}
	metadata, err := storage.FSStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = storage.required
	return metadata, err
}

func (storage *documentQueryScopeStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.snapshots.Add(1)
	if err := storage.check(ctx); err != nil {
		return domain.Snapshot{}, err
	}
	return storage.FSStore.GetSnapshot(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) checkPayload(ctx context.Context) error {
	storage.payloads.Add(1)
	if err := storage.check(ctx); err != nil {
		return err
	}
	return storage.payloadError
}

func (storage *documentQueryScopeStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.SessionDoc{}, err
	}
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return storage.FSStore.GetDocManifest(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return nil, err
	}
	return storage.FSStore.GetChunk(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.DocReadIndex{}, err
	}
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return nil, err
	}
	return storage.FSStore.SearchDocEvents(ctx, repo, hash, query, after, limit)
}

type documentQueryScopeFailedBlobs struct {
	outbound.BlobStore
	calls atomic.Int64
	err   error
}

func (storage *documentQueryScopeFailedBlobs) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.calls.Add(1)
	return domain.SessionDoc{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.calls.Add(1)
	return domain.DocChunkManifest{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.calls.Add(1)
	return nil, storage.err
}

func (storage *documentQueryScopeFailedBlobs) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.calls.Add(1)
	return domain.DocReadIndex{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	storage.calls.Add(1)
	return nil, storage.err
}

type documentQueryScopeFailedMetadata struct {
	outbound.MetadataStore
	calls atomic.Int64
	err   error
}

func (storage *documentQueryScopeFailedMetadata) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	storage.calls.Add(1)
	return domain.Repo{}, storage.err
}

func (storage *documentQueryScopeFailedMetadata) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	storage.calls.Add(1)
	return domain.Snapshot{}, storage.err
}

func TestDocumentEventQueriesVerifiedProfileRequiresPinnedTransactions(test *testing.T) {
	filesystem := store.NewFSStore(test.TempDir())
	service := NewService(filesystem, filesystem, nil, nil)
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.VerifiedRoots); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("verified profile accepted a store without pinned repository transactions", queries, err)
	}
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.LegacyCompatibility); queries == nil || err != nil {
		test.Fatal("explicit native compatibility was disabled", queries, err)
	}
}
