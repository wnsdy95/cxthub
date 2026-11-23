package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewCountStore struct {
	*store.FSStore
	chunkReads, fullReads, pageReads int
}

func (storage *docViewCountStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	return storage.FSStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (storage *docViewCountStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	storage.fullReads++
	return storage.FSStore.ReadVerifiedDoc(ctx, repo, hash)
}

func (storage *docViewCountStore) DocRangePage(ctx context.Context, repo domain.ContentHash, index domain.DocRangeIndex, page int) ([]domain.DocRangeEntry, error) {
	storage.pageReads++
	return storage.FSStore.DocRangePage(ctx, repo, index, page)
}

type docViewSnapshotStore struct {
	*docViewCountStore
	readonly bool
	opens    int
	pages    int
	openErr  error
}

type docViewSnapshotRead struct {
	store  *docViewSnapshotStore
	repo   domain.ContentHash
	header domain.DocRangeIndex
}

func (storage *docViewSnapshotStore) InReadOnlyTransaction(context.Context) bool {
	return storage.readonly
}

func (storage *docViewSnapshotStore) OpenDocRange(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeRead, error) {
	storage.opens++
	if storage.openErr != nil {
		return nil, storage.openErr
	}
	header, err := storage.FSStore.DocRangeIndex(ctx, repo, hash)
	return &docViewSnapshotRead{store: storage, repo: repo, header: header}, err
}

func (read *docViewSnapshotRead) Header() domain.DocRangeIndex { return read.header }

func (read *docViewSnapshotRead) ReadPage(ctx context.Context, page int) ([]domain.DocRangeEntry, error) {
	read.store.pages++
	return read.store.FSStore.DocRangePage(ctx, read.repo, read.header, page)
}
