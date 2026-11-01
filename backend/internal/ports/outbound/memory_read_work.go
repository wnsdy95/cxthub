package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadWorkStore interface {
	ConfigureMemoryReadWork(context.Context, domain.ContentHash, bool) error
	RequestMemoryReadWork(context.Context, domain.MemoryReadTarget) (domain.MemoryReadWork, error)
	ReadMemoryReadWork(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryReadWork, error)
	ClaimMemoryReadWork(context.Context, []domain.ContentHash, time.Duration) (domain.MemoryReadWork, error)
	RenewMemoryReadWork(context.Context, domain.MemoryReadWork, time.Duration) error
	FailMemoryReadWork(context.Context, domain.MemoryReadWork, string, bool) error
	FinishMemoryReadWork(context.Context, domain.MemoryReadWork, domain.MemoryReadQualification) (domain.ContentHash, error)
}

type CurrentMemoryReadStore interface {
	WithCurrentMemoryReadRange(context.Context, domain.MemoryReadTarget, func(context.Context) (context.Context, error), func(context.Context, domain.MemoryReadHeader, MemoryReadChunkReader) error) error
}

type MemoryReadWorkReconciler interface {
	ReconcileMemoryReadWork(context.Context, []domain.ContentHash, int) (int, error)
}

type MemoryReadObjectWakeDispatcher interface {
	DispatchMemoryReadObjectWakes(context.Context, int) (int, error)
}
