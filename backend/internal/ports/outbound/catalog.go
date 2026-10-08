package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CatalogStore is optional. Implementations must read committed immutable
// journal images at a fixed upper sequence, bind opaque cursors to repository,
// scope, version and epoch, and publish checkpoints only on the final page.
// Stores without this capability must not approximate it with mutable scans.
type CatalogStore interface {
	CatalogChanges(context.Context, domain.ContentHash, domain.CatalogRequest) (domain.CatalogPage, error)
}
