package documentquery_test

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootFixtureScope struct{}

func (rootFixtureScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (rootFixtureScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (rootFixtureScope) WithinDocumentRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

func (rootFixtureScope) WithinDocumentSnapshot(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

type rootFixtureProjectionCall struct {
	hash       domain.ContentHash
	hashesOnly bool
}

type rootFixtureReader struct {
	*store.FSStore
	snapshotFailures       map[domain.ContentHash]error
	projectionReplacements map[domain.ContentHash]domain.ContentHash
	projectionFailures     map[domain.ContentHash]error
	snapshotCalls          []domain.ContentHash
	projectionCalls        []rootFixtureProjectionCall
	verifiedCalls          []domain.ContentHash
	fallbackCalls          []string
}

func (reader *rootFixtureReader) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	reader.snapshotCalls = append(reader.snapshotCalls, hash)
	snapshot, err := reader.FSStore.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return snapshot, err
	}
	return snapshot, reader.snapshotFailures[hash]
}

func (reader *rootFixtureReader) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	reader.projectionCalls = append(reader.projectionCalls, rootFixtureProjectionCall{hash: hash, hashesOnly: selection.HashesOnly})
	requested := hash
	if replacement, exists := reader.projectionReplacements[hash]; exists {
		if replacement == "" {
			return domain.VerifiedRootEvents{}, nil
		}
		hash = replacement
	}
	projection, err := reader.FSStore.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if err != nil {
		return projection, err
	}
	return projection, reader.projectionFailures[requested]
}

func (reader *rootFixtureReader) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	reader.verifiedCalls = append(reader.verifiedCalls, hash)
	return reader.FSStore.ReadVerifiedDoc(ctx, repo, hash)
}

func (reader *rootFixtureReader) rejectFallback(method string) error {
	reader.fallbackCalls = append(reader.fallbackCalls, method)
	return fmt.Errorf("root query attempted legacy fallback: %s", method)
}

func (reader *rootFixtureReader) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, reader.rejectFallback("GetDoc")
}

func (reader *rootFixtureReader) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, reader.rejectFallback("GetDocManifest")
}

func (reader *rootFixtureReader) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, reader.rejectFallback("GetChunk")
}

func (reader *rootFixtureReader) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, reader.rejectFallback("DocReadIndex")
}

func (reader *rootFixtureReader) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, reader.rejectFallback("SearchDocEvents")
}
