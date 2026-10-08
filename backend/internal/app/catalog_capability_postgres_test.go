//go:build postgres

package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestPostgresCatalogVersion(t *testing.T) {
	// Capability discovery needs no live connection: exercise the actual PG
	// adapter's method set and ensure it does not use the uninitialized pool.
	st := &store.PostgresStore{}
	if got := NewService(st, st, nil, nil, nil).CatalogVersion(); got != 1 {
		t.Fatalf("PostgreSQL CatalogVersion() = %d, want 1", got)
	}
}
