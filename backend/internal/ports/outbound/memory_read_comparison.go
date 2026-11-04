package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadComparisonStore interface {
	RecordMemoryReadComparison(context.Context, domain.MemoryReadWork, domain.MemoryReadComparison) error
}
