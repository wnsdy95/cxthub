package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPublishedGraphQueriesKeepOriginalReadersWithoutServiceBackReference(test *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(test)
	queries, err := service.PublishedGraphQueries()
	if err != nil {
		test.Fatal(err)
	}
	if _, owned := queries.(*repositoryquery.PublishedGraphQueries); !owned {
		test.Fatal("published graph is not owned by its query implementation")
	}
	service.meta = store.NewFSStore(test.TempDir())
	view, err := queries.GetRepositoryView(systemTestContext(), repo)
	if err != nil || view.ReadGeneration == nil || !view.ReadGeneration.Complete || storage.reads != 1 || storage.snapshotLists != 0 || storage.evidenceReads != 0 {
		test.Fatal("published query rediscovered the parent Service or loaded live graph inputs", view.ReadGeneration, err, storage.reads)
	}
}

func TestPublishedGraphScopeNeverRepairsReadonlyOrWritingContext(test *testing.T) {
	service, _, repo, _ := newGraphProjectionRead(test)
	scope := &publishedGraphQueryScope{repository: &repositoryQueryScope{metadata: service.meta, supported: service.DocumentIdentitiesSupported()}}
	for _, ctx := range []context.Context{outbound.WithDocReadOnly(systemTestContext()), context.WithValue(systemTestContext(), afterCommitKey{}, &afterCommitActions{})} {
		called := false
		err := scope.RepairGraph(ctx, repo, func(context.Context) error { called = true; return nil })
		if scope.GraphRepairAllowed(ctx) || !errors.Is(err, domain.ErrConflict) || called {
			test.Fatal("read-only/write owner received derived repair", err, called)
		}
	}
	scope.transactional = true
	called := false
	err := scope.RepairGraph(systemTestContext(), repo, func(context.Context) error { called = true; return nil })
	if scope.GraphRepairAllowed(systemTestContext()) || !errors.Is(err, domain.ErrConflict) || called {
		test.Fatal("unknown borrowed transaction ownership scheduled repair", err, called)
	}
}
