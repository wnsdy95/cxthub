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

// SnapshotCatalogReader optionally avoids presentation ordering for callers
// that canonicalize a complete catalog themselves. Every call must freshly read
// and validate the same metadata as ListSnapshots(ctx, repo, ""); it must not
// cache mutable attachments, create files, take mutation locks or repair state.
// The result has no ordering guarantee and provides no atomic-read guarantee.
type SnapshotCatalogReader interface {
	ListSnapshotCatalog(context.Context, string) ([]domain.Snapshot, error)
}

type LocalBranchReader interface {
	ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error)
}

type LocalRefReader interface {
	ListRefs(context.Context, string) ([]domain.Ref, error)
	StashList(context.Context, string) ([]domain.StashEntry, error)
}
