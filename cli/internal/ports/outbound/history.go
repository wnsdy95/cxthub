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

type WorkingPositionStore interface {
	GetWorkingPosition(context.Context) (domain.WorkingPosition, error)
	PutWorkingPosition(context.Context, domain.WorkingPosition) error
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
