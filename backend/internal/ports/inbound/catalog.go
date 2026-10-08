package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CatalogCapabilities optionally advertises runtime catalog support. Zero means
// unsupported. It is separate from CatalogQuery so query implementations do not
// have to advertise a capability.
type CatalogCapabilities interface {
	CatalogVersion() int
}

// CatalogQuery is an optional synchronization metadata query. Transports must
// authorize pull access on every call, including continuation and empty pages.
type CatalogQuery interface {
	CatalogChanges(context.Context, domain.ContentHash, domain.CatalogRequest) (domain.CatalogPage, error)
}
