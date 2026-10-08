package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CatalogQuery is an optional synchronization metadata query. Transports must
// authorize pull access on every call, including continuation and empty pages.
type CatalogQuery interface {
	CatalogChanges(context.Context, domain.ContentHash, domain.CatalogRequest) (domain.CatalogPage, error)
}
