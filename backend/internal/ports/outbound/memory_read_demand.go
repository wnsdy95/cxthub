package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadDemandStore interface {
	RequestMinimumMemoryReadWork(context.Context, domain.MemoryReadTarget, domain.MemoryReadRequirement) (domain.MemoryReadWork, error)
}
