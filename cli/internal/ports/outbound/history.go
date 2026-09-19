package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// History records are immutable and independent of provider session files.
type HistoryStore interface {
	PutHistoryEvent(context.Context, domain.HistoryEvent) error
	ListHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error)
}

type LocalBranchStore interface {
	ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error)
	BindLocalBranch(context.Context, domain.HistoryEvent) error
	RenameLocalBranch(context.Context, string, string, string) error
	UnbindLocalBranch(context.Context, string, string) error
}

type RemoteHistory interface {
	PushHistoryEvent(context.Context, domain.HistoryEvent) error
	PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error)
}

type WorkingPositionReader interface {
	GetWorkingPosition(context.Context) (domain.WorkingPosition, error)
}

type WorkingPositionStore interface {
	WorkingPositionReader
	PutWorkingPosition(context.Context, domain.WorkingPosition) error
}

// WorkingPositionCASStore atomically compares the entire expected position and
// branch ref before replacing only this worktree's position. Stale observations
// return ErrSyncConflict, including a different Git branch/commit observed when
// constructing the worktree store. Selection history and retention are durable.
type WorkingPositionCASStore interface {
	CompareAndSwapWorkingPosition(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
}

type WorkingBranchRenameStore interface {
	RenameWorkingBranch(context.Context, string, string, string) error
}

type WorkingMemoryStore interface {
	RecordWorkingMemory(context.Context, domain.ContentHash, domain.ContentHash) error
}

// WorkingCommitStore publishes a selected continuation with a local ref CAS.
// The implementation journals retention and position updates before moving it.
type WorkingCommitStore interface {
	CommitWorkingSnapshot(context.Context, domain.Ref, domain.ContentHash, domain.WorkingPosition, *domain.HistoryEvent) error
}

// WorkingCommitCASStore also protects the worktree selection read by Save.
// Both the ref and complete expected position must match before journaling;
// memory-only repins and explicit selections are concurrent writes too.
type WorkingCommitCASStore interface {
	CommitWorkingSnapshotIfCurrent(context.Context, domain.Ref, domain.ContentHash, domain.WorkingPosition, domain.WorkingPosition, *domain.HistoryEvent) error
}
