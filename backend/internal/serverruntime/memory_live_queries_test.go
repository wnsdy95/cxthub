package serverruntime

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
)

func TestLiveMemoryRuntimeOwnsActualReaderAndPreservesPreviousOnFailure(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	runtime := &Runtime{Store: storage, Context: app.NewService(storage, storage, nil, nil, nil)}
	if err := runtime.ConfigureLiveMemoryQueries(); err != nil {
		test.Fatal(err)
	}
	if _, owned := runtime.MemoryLive.(*memoryquery.LiveQueries); !owned {
		test.Fatal("live memory root is not owned by the actual query component")
	}
	previous := runtime.MemoryLive
	runtime.Context = app.NewService(storage, nil, nil, nil, nil)
	if err := runtime.ConfigureLiveMemoryQueries(); err == nil || runtime.MemoryLive != previous {
		test.Fatal("missing projection reader replaced the previous live root", err)
	}
	runtime.Context = nil
	if err := runtime.ConfigureLiveMemoryQueries(); err == nil || runtime.MemoryLive != previous {
		test.Fatal("nil context replaced the previous live root", err)
	}
}
