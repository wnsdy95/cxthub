package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// HistoricalBackfillStore must be paired with ObjectRetention. Queueing holds
// a retained-object lease; collection checks pins under its exclusive lease.
// The worker lock spans network work, but queue writes never hold that lock.
type HistoricalBackfillStore interface {
	StageBackfills(context.Context, string, []domain.Snapshot) error
	ListBackfills(context.Context, string) ([]domain.SnapshotBackfill, error)
	HasBackfillPin(context.Context, domain.ContentHash) (bool, error)
	UpdateBackfill(context.Context, domain.SnapshotBackfill, *domain.SnapshotBackfill) error
	WithBackfillWorker(context.Context, func() error) (bool, error)
}
