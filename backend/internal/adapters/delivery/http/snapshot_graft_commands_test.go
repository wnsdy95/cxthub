package http

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type snapshotGraftHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*snapshotGraftHTTPBackend) GraftSnapshotParents(context.Context, domain.ContentHash, domain.ContentHash, []domain.ContentHash, uint64) error {
	panic("graft reached broad HTTP backend")
}

type snapshotGraftHTTPPorts struct {
	test     *testing.T
	bound    context.Context
	repo     domain.ContentHash
	target   domain.ContentHash
	parents  []domain.ContentHash
	expected uint64
	calls    int
	err      error
}

func (ports *snapshotGraftHTTPPorts) WithinSnapshotGraftWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	ports.test.Helper()
	if actor, system := inbound.RepositoryActor(ctx); actor != "actor" || system {
		ports.test.Fatal("graft lost authenticated actor", actor, system)
	}
	ports.bound = ctx
	defer func() { ports.bound = nil }()
	return write(ctx)
}

func (ports *snapshotGraftHTTPPorts) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	ports.test.Helper()
	if ctx != ports.bound {
		ports.test.Fatal("graft read escaped scope")
	}
	return domain.Snapshot{ID: id, RepoID: repo}, nil
}

func (ports *snapshotGraftHTTPPorts) AddGraftParentsCAS(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, expected uint64) error {
	ports.test.Helper()
	if ctx != ports.bound {
		ports.test.Fatal("graft CAS escaped scope")
	}
	ports.repo, ports.target, ports.parents, ports.expected = repo, id, parents, expected
	ports.calls++
	return ports.err
}
