package http

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestLiveMemorySetterRejectsAbsentReaderAndPreservesPrevious(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	service := app.NewService(storage, storage, nil, nil, nil)
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	server := NewServer(service, nil)
	if err := server.SetLiveMemoryQueries(queries); err != nil {
		test.Fatal(err)
	}
	var missing *memoryquery.LiveQueries
	for _, reader := range []inbound.LiveMemoryQueries{nil, missing} {
		if err := server.SetLiveMemoryQueries(reader); err == nil || server.effectiveMemory != queries || server.memoryPositions != queries {
			test.Fatal("absent reader replaced the live query and resource scope", err)
		}
	}
}
