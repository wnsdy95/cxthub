package serverruntime

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRepositoryStateRuntimeProfileIsIndependentOfDocuments(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	runtime := &Runtime{Store: storage, Context: app.NewService(storage, storage, nil, nil, nil)}
	for _, documentProfile := range []inbound.DocumentQueryProfile{0, inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
		runtime.DocumentQueryProfile = documentProfile
		if err := runtime.ConfigureRepositoryQueries(); err != nil {
			test.Fatal(err)
		}
		if _, owned := runtime.RepositoryStates.(*repositoryquery.Queries); !owned || runtime.RepositoryStateProfile != inbound.RepositoryStateCompatibility || runtime.DocumentQueries != nil {
			test.Fatal("repository state root depends on an unrelated document reader", runtime.RepositoryStateProfile, documentProfile)
		}
		if _, owned := runtime.RepositoryGraphs.(*repositoryquery.GraphQueries); !owned || runtime.RepositoryGraphProfile != inbound.RepositoryGraphCompatibility {
			test.Fatal("repository graph root is not an independently assembled query", runtime.RepositoryGraphProfile)
		}
		if _, owned := runtime.ContextQueries.(*repositoryquery.ContextQueries); !owned || runtime.ContextQueryProfile != inbound.ContextQueryCompatibility {
			test.Fatal("repository context root depends on document configuration", runtime.ContextQueryProfile)
		}
	}
	previous, profile := runtime.RepositoryStates, runtime.RepositoryStateProfile
	previousGraphs, graphProfile := runtime.RepositoryGraphs, runtime.RepositoryGraphProfile
	previousContexts, contextProfile := runtime.ContextQueries, runtime.ContextQueryProfile
	runtime.Context = app.NewService(storage, nil, nil, nil, nil)
	if err := runtime.ConfigureRepositoryQueries(); err == nil || runtime.RepositoryStates != previous || runtime.RepositoryGraphs != previousGraphs || runtime.ContextQueries != previousContexts {
		test.Fatal("failed context construction replaced an earlier successful read root", err)
	}
	runtime.Context = app.NewService(nil, nil, nil, nil, nil)
	if err := runtime.ConfigureRepositoryQueries(); err == nil || runtime.RepositoryStates != previous || runtime.RepositoryStateProfile != profile {
		test.Fatal("failed repository query assembly replaced the active root", err)
	}
	if runtime.RepositoryGraphs != previousGraphs || runtime.RepositoryGraphProfile != graphProfile {
		test.Fatal("failed graph query assembly replaced the active root")
	}
	if runtime.ContextQueries != previousContexts || runtime.ContextQueryProfile != contextProfile {
		test.Fatal("failed context query assembly replaced the active root")
	}
}
