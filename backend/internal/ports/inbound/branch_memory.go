package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// BranchMemoryQuery shares verified inclusion with the graph and effective
// memory. A blank code requests an unambiguous recorded association only.
type BranchMemoryQuery interface {
	QueryBranchMemory(context.Context, domain.ContentHash, string, domain.ContentHash, string) (domain.MemoryProjection, error)
}
