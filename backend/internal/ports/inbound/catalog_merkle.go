package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CatalogMerkleQuery requires fresh pull authorization on every root/node page.
type CatalogMerkleQuery interface {
	CatalogMerkle(context.Context, domain.ContentHash, domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error)
}
type CatalogMerkleCapabilities interface{ CatalogMerkleVersion() int }
