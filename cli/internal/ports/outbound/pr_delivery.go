package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type PRDeliveryStore interface {
	AttemptPRDelivery(context.Context, string, domain.PullRequestMerge) error
	QueuePRDelivery(context.Context, string, domain.PullRequestMerge) error
	PendingPRDeliveries(context.Context, string) ([]domain.PullRequestMerge, error)
	AcceptPRDelivery(context.Context, string, domain.PullRequestMerge) error
}
