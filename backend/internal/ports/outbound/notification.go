package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type NotificationDelivery struct {
	Job         domain.NotificationJob `json:"job"`
	Destination string                 `json:"destination"`
}

type NotificationStore interface {
	EnqueueNotification(context.Context, NotificationDelivery) error
	ListNotifications(context.Context, string) ([]domain.NotificationJob, error)
	ClaimNotification(context.Context, time.Time, time.Duration) (NotificationDelivery, error)
	FinishNotification(context.Context, domain.NotificationJob, time.Time) error
	RetryNotification(context.Context, string, string, string, time.Time) error
}

// WorkspaceTransactions joins membership changes and their outbox entry.
type WorkspaceTransactions interface {
	WithinWorkspace(context.Context, string, func(context.Context) error) error
}
