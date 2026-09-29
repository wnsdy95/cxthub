package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// StagingStore uses the repository ref transaction lock for index mutations and
// commit acceptance. ReadStaging does not replay or create state.
type StagingStore interface {
	ReadStaging(context.Context, string) (domain.StagingIndex, domain.WorkingPosition, error)
	CompareAndSwapStaging(context.Context, domain.ContentHash, domain.StagingIndex, domain.WorkingPosition) error
	FinalizeStagingCommit(context.Context, domain.StagingCommit) (domain.StagingCommit, error)
	ResumeStagingCommit(context.Context, string, string) (domain.StagingCommit, error)
	ListStagingCommits(context.Context, string) ([]domain.StagingCommit, error)
}

// StagingPins is consulted under object retention before deleting a document.
// It includes every worktree index and unfinished/finalized operation manifest.
type StagingPins interface {
	HasStagingPin(context.Context, domain.ContentHash) (bool, error)
}

// StagingStashStore keeps saved manifests after an applied pop as immutable
// retention evidence; Applied stashes are omitted from the pending stash list.
type StagingStashStore interface {
	StashIndex(context.Context, domain.StagingStash) (domain.StagingStash, error)
	PopIndex(context.Context, string, string, domain.ContentHash, domain.WorkingPosition) (domain.StagingIndex, error)
	ListIndexStashes(context.Context, string) ([]domain.StagingStash, error)
}
