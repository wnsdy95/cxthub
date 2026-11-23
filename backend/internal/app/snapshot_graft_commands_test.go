package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type snapshotGraftStoreProbe struct {
	test     *testing.T
	repo     domain.ContentHash
	reads    []domain.ContentHash
	readErr  map[domain.ContentHash]error
	target   domain.ContentHash
	parents  []domain.ContentHash
	expected uint64
	writes   int
	writeErr error
	observe  func(context.Context)
}

func (storage *snapshotGraftStoreProbe) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	storage.test.Helper()
	storage.observe(ctx)
	if repo != storage.repo {
		storage.test.Fatalf("snapshot read changed repository: %s", repo)
	}
	storage.reads = append(storage.reads, id)
	return domain.Snapshot{ID: id, RepoID: repo}, storage.readErr[id]
}

func (storage *snapshotGraftStoreProbe) AddGraftParentsCAS(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, expected uint64) error {
	storage.test.Helper()
	storage.observe(ctx)
	if repo != storage.repo {
		storage.test.Fatalf("CAS changed repository: %s", repo)
	}
	storage.target, storage.parents, storage.expected = id, append([]domain.ContentHash(nil), parents...), expected
	storage.writes++
	return storage.writeErr
}

type snapshotGraftScopeProbe struct {
	bound context.Context
	calls int
	err   error
}

func (scope *snapshotGraftScopeProbe) WithinSnapshotGraftWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	scope.calls++
	if scope.err != nil {
		return scope.err
	}
	scope.bound = context.WithValue(ctx, struct{ graft bool }{true}, repo)
	defer func() { scope.bound = nil }()
	return write(scope.bound)
}
