package serverruntime

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
)

func TestMemorySnapshotRuntimeOwnsActualReaderAndPreservesPreviousOnFailure(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	runtime := &Runtime{Store: storage, Context: app.NewService(storage, storage, nil, nil, nil)}
	if err := runtime.ConfigureMemorySnapshotQueries(); err != nil {
		test.Fatal(err)
	}
	if _, owned := runtime.MemorySnapshots.(*memoryquery.SnapshotQueries); !owned {
		test.Fatal("memory snapshot root is not owned by the actual query component")
	}
	previous := runtime.MemorySnapshots
	runtime.Context = app.NewService(storage, nil, nil, nil, nil)
	if err := runtime.ConfigureMemorySnapshotQueries(); err == nil || runtime.MemorySnapshots != previous {
		test.Fatal("missing active reader dependency replaced the previous root", err)
	}
	runtime.Context = nil
	if err := runtime.ConfigureMemorySnapshotQueries(); err == nil || runtime.MemorySnapshots != previous {
		test.Fatal("nil context replaced the previous memory snapshot root", err)
	}
}
