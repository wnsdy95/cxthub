package serverruntime

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
)

func TestPreparedMemoryRuntimeOwnsExplicitQueryAndPreservesPreviousRoot(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	runtime := &Runtime{Store: storage, Context: app.NewService(storage, storage, nil, nil, nil)}
	if err := runtime.ConfigurePreparedMemoryQueries(); err != nil {
		test.Fatal(err)
	}
	if _, owned := runtime.PreparedMemory.(*memoryquery.PreparedQueries); !owned {
		test.Fatal("prepared memory is still assembled from the broad Service")
	}
	previous := runtime.PreparedMemory
	runtime.Context = nil
	if err := runtime.ConfigurePreparedMemoryQueries(); err == nil || runtime.PreparedMemory != previous {
		test.Fatal("failed query assembly replaced the active prepared reader", err)
	}
}
