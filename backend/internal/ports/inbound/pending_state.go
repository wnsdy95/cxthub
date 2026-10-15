package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PendingStateQuery interface {
	GetPendingState(context.Context, domain.ContentHash) (domain.PendingStateView, error)
}
