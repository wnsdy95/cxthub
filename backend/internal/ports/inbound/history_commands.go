package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type HistoryCommands interface {
	RecordHistory(context.Context, domain.HistoryEvent) error
}
