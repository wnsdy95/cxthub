package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// SnapshotListReader is the inspection capability used by local CLI queries.
type SnapshotListReader interface {
	ListSnapshots(context.Context, string, string) ([]domain.Snapshot, error)
	ListRefs(context.Context, string) ([]domain.Ref, error)
}

type LocalBranchReader interface {
	ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error)
}

type LocalRefReader interface {
	ListRefs(context.Context, string) ([]domain.Ref, error)
	StashList(context.Context, string) ([]domain.StashEntry, error)
}
