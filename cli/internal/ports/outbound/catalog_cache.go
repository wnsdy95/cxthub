package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// CatalogCache is acquisition progress, never verified/applied repository state.
// Entries is the last complete image; Pending is an unfinished fixed-bound run.
// Remote excludes credentials. Both parts are verified from current disk bytes.
type CatalogCache struct {
	Version    int
	RepoID     string
	Remote     string
	Revision   domain.ContentHash
	Checkpoint *domain.CatalogCheckpoint
	Entries    []domain.CatalogEntry
	Pending    []domain.CatalogPage
}

// CatalogCacheStore atomically stages pages and advances complete state only on
// a final checkpoint. Concurrent writers must match Revision; corruption is an
// error, never permission to overwrite. A reset abandons only unfinished pages.
type CatalogCacheStore interface {
	ReadCatalogCache(context.Context, string, string) (CatalogCache, error)
	AppendCatalogPage(context.Context, domain.ContentHash, string, string, domain.CatalogPage) (domain.ContentHash, error)
	ResetCatalogRun(context.Context, domain.ContentHash, string, string) (domain.ContentHash, error)
}
