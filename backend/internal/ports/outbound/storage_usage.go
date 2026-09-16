package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

// StorageAccounting is authoritative in production PostgreSQL. Reconciliation
// records corrections, never deletes customer objects or rewrites prior entries.
type StorageAccounting interface {
	ReadStorageUsage(context.Context, string, time.Time, time.Time) (domain.StorageUsage, error)
	ReconcileStorageUsage(context.Context, string) error
	ConfigureStoragePolicy(context.Context, string, string, int64, domain.StoragePolicy, string, string) error
}
