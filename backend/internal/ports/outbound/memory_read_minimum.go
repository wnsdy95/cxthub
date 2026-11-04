package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MinimumMemoryReadStore interface {
	ObserveMemoryReadRequirement(context.Context, domain.MemoryReadTarget, func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error)
	WithMinimumMemoryReadRange(context.Context, domain.MemoryReadTarget, domain.MemoryReadRequirement, domain.ContentHash, func(context.Context) (context.Context, error), func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, MemoryReadChunkReader) error) error
}

type MinimumMemoryReadWorkStore interface {
	FinishMinimumMemoryReadWork(context.Context, domain.MemoryReadWork, domain.MemoryReadQualification) (domain.ContentHash, error)
}
