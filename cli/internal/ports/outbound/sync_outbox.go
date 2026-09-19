package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// GraftQueueAccess is scoped to WithGrafts. Store is durable before it returns;
// callers can enforce queue-before-snapshot publication. Do not retain the
// access object or perform network I/O while its lock is held.
type GraftQueueAccess interface {
	Load() ([]domain.GraftQueueEvent, error)
	Store([]domain.GraftQueueEvent) error
}

// SyncOutbox owns local persistence/locking, never remote conflict policy.
type SyncOutbox interface {
	WithGrafts(ctx context.Context, root string, fn func(GraftQueueAccess) error) error
	EnqueuePromotion(ctx context.Context, root string, id domain.ContentHash, message string) error
	ListPromotions(ctx context.Context, root string) (map[domain.ContentHash]string, error)
	AcknowledgePromotion(ctx context.Context, root string, id domain.ContentHash, message string) error
}
