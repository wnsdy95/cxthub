package mcp

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedMemoryScopeFunc func(context.Context, domain.ContentHash, func(context.Context) error) error

func (scope preparedMemoryScopeFunc) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope(ctx, repo, read)
}

type preparedMemoryTestBackend struct {
	fakeContextBackend
	refs          []domain.Ref
	refReads      int
	snapshotReads int
	denyReads     bool
}

func (backend *preparedMemoryTestBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	backend.refReads++
	if backend.denyReads {
		return nil, errors.New("changed refs must not be resolved")
	}
	return backend.refs, nil
}

func (backend *preparedMemoryTestBackend) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	backend.snapshotReads++
	if backend.denyReads {
		return domain.Snapshot{}, errors.New("changed snapshots must not be resolved")
	}
	return backend.fakeContextBackend.GetSnapshot(ctx, repo, snapshot)
}
