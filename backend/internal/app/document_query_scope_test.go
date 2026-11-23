package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocumentEventQueriesVerifiedProfileRequiresPinnedTransactions(test *testing.T) {
	filesystem := store.NewFSStore(test.TempDir())
	service := NewService(filesystem, filesystem, nil, nil)
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.VerifiedRoots); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("verified profile accepted a store without pinned repository transactions", queries, err)
	}
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.LegacyCompatibility); queries == nil || err != nil {
		test.Fatal("explicit native compatibility was disabled", queries, err)
	}
}
