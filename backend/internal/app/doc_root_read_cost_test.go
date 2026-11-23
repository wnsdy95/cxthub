package app

import (
	"context"
	"runtime"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootReadCostStore struct {
	*store.FSStore
	reads        []domain.ContentHash
	fullReads    int
	measure      bool
	peakLive     uint64
	peakObserved uint64
}

func (reader *rootReadCostStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	reader.fullReads++
	reader.reads = append(reader.reads, hash)
	doc, err := reader.FSStore.ReadVerifiedDoc(ctx, repo, hash)
	if reader.measure {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		reader.peakObserved = max(reader.peakObserved, stats.HeapAlloc)
		runtime.GC()
		runtime.ReadMemStats(&stats)
		reader.peakLive = max(reader.peakLive, stats.HeapAlloc)
		runtime.KeepAlive(doc)
	}
	return doc, err
}

func (reader *rootReadCostStore) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	reader.reads = append(reader.reads, hash)
	projection, err := reader.FSStore.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if reader.measure {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		reader.peakObserved = max(reader.peakObserved, stats.HeapAlloc)
		runtime.GC()
		runtime.ReadMemStats(&stats)
		reader.peakLive = max(reader.peakLive, stats.HeapAlloc)
		runtime.KeepAlive(projection)
	}
	return projection, err
}
