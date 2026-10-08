package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestCatalogVersionRequiresCatalogAndReadTransactions(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta outbound.MetadataStore
		want int
	}{
		{"neither", struct{ outbound.MetadataStore }{}, 0},
		{"catalog_only", struct {
			outbound.MetadataStore
			outbound.CatalogStore
		}{}, 0},
		{"transactions_only", struct {
			outbound.MetadataStore
			outbound.RepositoryTransactions
		}{}, 0},
		{"catalog_and_transactions", struct {
			outbound.MetadataStore
			outbound.CatalogStore
			outbound.RepositoryTransactions
		}{}, 1},
		{"filesystem", store.NewFSStore(t.TempDir()), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Nil embedded ports also ensure discovery never queries the store
			// or opens a transaction merely to advertise its capabilities.
			var capabilities inbound.CatalogCapabilities = NewService(tc.meta, nil, nil, nil, nil)
			if got := capabilities.CatalogVersion(); got != tc.want {
				t.Fatalf("CatalogVersion() = %d, want %d", got, tc.want)
			}
		})
	}
}
