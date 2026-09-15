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
