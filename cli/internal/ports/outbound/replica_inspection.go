package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type LocalReplicaInspector interface {
	ResolveRepo(context.Context, string, string) (domain.Repo, error)
	InspectReplica(context.Context, string) domain.ReplicaInspection
	ListBackfills(context.Context, string, string) ([]domain.SnapshotBackfill, error)
}
