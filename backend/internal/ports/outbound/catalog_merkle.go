package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CatalogMerkleStore derives immutable, checkpoint-bound metadata trees from
// committed catalog images. It owns its read snapshots and publishes only its
// separate derived cache, never repository source state. Caller-owned write or
// read transactions must be rejected rather than bypassed.
type CatalogMerkleStore interface {
	CatalogMerkle(context.Context, domain.ContentHash, domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error)
}
