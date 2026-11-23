package outbound

// CatalogCacheStore atomically stages pages and advances complete state only on
// a final checkpoint. Concurrent writers must match Revision; corruption is an
// error, never permission to overwrite. A reset abandons only unfinished pages.
type CatalogCacheStore interface {
}
