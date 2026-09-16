package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// DocReadStore indexes verified content without changing the archival blob.
// Implementations must check repository ownership even when projections exist.
type DocReadStore interface {
	DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error)
	SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error)
}
