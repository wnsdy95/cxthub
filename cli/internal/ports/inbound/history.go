package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ContextHistory interface {
	ValidateHistorySource(context.Context, domain.HistoryEvent) (domain.HistoryEvent, error)
	RecordHistory(context.Context, domain.HistoryEvent) error
	ListHistory(context.Context, string) ([]domain.HistoryEvent, error)
	SelectPosition(context.Context, domain.WorkingPosition) error
	CurrentPosition(context.Context) (domain.WorkingPosition, error)
	ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error)
	BindLocalBranch(context.Context, domain.HistoryEvent) error
}

// ContextHistoryReconciler is optional. The caller proves that a recorded code
// transition authorizes next; this port validates the selection and applies it
// only while the complete worktree position and shared branch ref remain exact.
// It neither discovers a target from the newest ref nor changes branch refs.
type ContextHistoryReconciler interface {
	SelectPositionIfCurrent(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
}

// ContextCodeMoveReconciler is distinct from same-code refresh. The caller
// proves the latest forward Git move before applying its completed PR source.
type ContextCodeMoveReconciler interface {
	SelectPositionAfterCodeMove(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
}
