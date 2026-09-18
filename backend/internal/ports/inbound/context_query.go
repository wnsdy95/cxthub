package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// ContextQuery is the read-only application boundary used by every delivery.
type ContextQuery interface {
	QueryContext(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error)
}
