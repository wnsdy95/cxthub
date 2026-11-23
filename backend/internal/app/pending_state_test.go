package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingStateStore struct {
	*store.FSStore
	snapshotLists int
	snapshotReads int
	evidenceReads int
}

func (storage *pendingStateStore) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	storage.snapshotLists++
	return storage.FSStore.ListSnapshots(ctx, repo, branch)
}

func (storage *pendingStateStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.snapshotReads++
	return storage.FSStore.GetSnapshot(ctx, repo, hash)
}
