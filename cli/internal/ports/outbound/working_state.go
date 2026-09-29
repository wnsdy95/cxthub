package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// WorkingStateReader is deliberately narrower than StagingStore and SessionStore.
// All methods must read without creating files, taking mutation locks, recovering
// journals, reopening provider sources, or inferring remote acknowledgements.
type WorkingStateReader interface {
	ReadStaging(context.Context, string) (domain.StagingIndex, domain.WorkingPosition, error)
	ListStagingCommits(context.Context, string) ([]domain.StagingCommit, error)
	ListPendings(context.Context, string) ([]domain.Pending, error)
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
}

type AppliedPullReader interface {
	ReadAppliedPull(context.Context, string, string) (SelectedPullReceipt, error)
}
