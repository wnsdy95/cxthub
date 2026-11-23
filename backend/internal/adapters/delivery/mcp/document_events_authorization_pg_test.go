//go:build postgres

package mcp

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentEventSnapshotPGBackend struct {
	*app.Service
	snapshot domain.Snapshot
	lookups  []context.Context
}

func (backend *documentEventSnapshotPGBackend) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if repo != backend.snapshot.RepoID || hash != backend.snapshot.ID {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return backend.snapshot, ctx.Err()
}

func (backend *documentEventSnapshotPGBackend) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	backend.lookups = append(backend.lookups, ctx)
	return backend.Service.GetRepo(ctx, repo)
}
