package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docViewCountStore struct {
	*store.FSStore
	chunkReads, fullReads, pageReads int
}

func (storage *docViewCountStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	return storage.FSStore.ReadConversationChunk(ctx, repo, hash, length)
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

func (read *docViewSnapshotRead) Header() domain.DocRangeIndex { return read.header }
