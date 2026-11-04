package main

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type localReplicaInspector struct{}

var _ outbound.LocalReplicaInspector = localReplicaInspector{}

func (localReplicaInspector) ResolveRepo(ctx context.Context, root, cwd string) (domain.Repo, error) {
	return remotecfg.Wrap(root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
}

func (localReplicaInspector) InspectReplica(ctx context.Context, root string) domain.ReplicaInspection {
	return storage.NewFileStore(root).InspectReplica(ctx)
}

func (localReplicaInspector) ListBackfills(ctx context.Context, root, repo string) ([]domain.SnapshotBackfill, error) {
	return storage.NewFileStore(root).ListBackfills(ctx, repo)
}
