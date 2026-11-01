package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ContextBranchOperations interface {
	Fork(context.Context, ForkInput) (ForkOutput, error)
	PreviewJoin(context.Context, JoinPreviewInput) (JoinPreviewOutput, error)
	ConfirmJoin(context.Context, ConfirmJoinInput) (JoinOutput, error)
	EnableContextProtocol(context.Context, domain.ContentHash) error
	ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}
