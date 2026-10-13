package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

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

func TestRootEventPagePreparesBaseBeforeRetainingTarget(test *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), test.TempDir()
	storage := store.NewFSStore(dir)
	base, _ := seedRootReadDoc(test, dir, storage, repo, historyMessage(domain.RoleUser, "shared prefix"))
	target, _ := seedRootReadDoc(test, dir, storage, repo, base.CIR.Events[0], historyMessage(domain.RoleAssistant, "new answer"))
	reader := &rootReadCostStore{FSStore: storage}
	service := NewService(storage, reader, nil, nil, nil)
	page, err := service.ReadDocEvents(ctx, repo, target.Hash, base.Hash, -1, 1)
	if err != nil || page.Inherited != 1 || page.Offset != 1 || page.Next != -1 || !reflect.DeepEqual(page.Events, target.CIR.Events[1:]) {
		test.Fatalf("base comparison changed: %+v %v", page, err)
	}
	if reader.fullReads != 0 || !reflect.DeepEqual(reader.reads, []domain.ContentHash{base.Hash, target.Hash}) {
		test.Fatalf("target body retained during base verification: %v", reader.reads)
	}
}

func TestRootEventPageBaseFirstPreservesPaging(test *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), test.TempDir()
	storage := store.NewFSStore(dir)
	service := NewService(storage, storage, nil, nil, nil)
	prefix := historyMessage(domain.RoleUser, "shared prefix")
	base, _ := seedRootReadDoc(test, dir, storage, repo, prefix)
	legacy := putHistoryDoc(test, storage, repo, historyEnvelope(), prefix)
	target, _ := seedRootReadDoc(test, dir, storage, repo, prefix, historyMessage(domain.RoleAssistant, "new answer"))
	empty, _ := seedRootReadDoc(test, dir, storage, repo)
	for _, testcase := range []struct {
		name                  string
		target, base          domain.ContentHash
		offset                int
		inherited, start, end int
		next                  int
	}{
		{"root-parent", target.Hash, base.Hash, -1, 1, 1, 2, -1},
		{"legacy-parent", target.Hash, legacy.Hash, -1, 1, 1, 2, -1},
		{"explicit-prefix", target.Hash, base.Hash, 0, 1, 0, 1, 1},
		{"explicit-tail", target.Hash, base.Hash, 1, 1, 1, 2, -1},
		{"end-offset", target.Hash, base.Hash, 2, 1, 2, 2, -1},
		{"same-root", target.Hash, target.Hash, -1, 2, 2, 2, -1},
		{"empty-target", empty.Hash, base.Hash, -1, 0, 0, 0, -1},
		{"empty-base", target.Hash, empty.Hash, -1, 0, 0, 1, 1},
		{"no-base", target.Hash, "", -1, 0, 0, 1, 1},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			page, err := service.ReadDocEvents(ctx, repo, testcase.target, testcase.base, testcase.offset, 1)
			want := append([]domain.CIREvent{}, target.CIR.Events[testcase.start:testcase.end]...)
			if err != nil || page.Inherited != testcase.inherited || page.Offset != testcase.start || page.Next != testcase.next || !reflect.DeepEqual(page.Events, want) {
				test.Fatalf("page changed: %+v %v", page, err)
			}
		})
	}
}

func TestRootEventPageBaseFirstRejectsUnreadCorruption(test *testing.T) {
	for _, source := range []string{"base", "target"} {
		for _, mutation := range []string{"changed", "missing"} {
			test.Run(source+"/"+mutation, func(test *testing.T) {
				ctx, repo, dir := systemTestContext(), h('1'), test.TempDir()
				storage := store.NewFSStore(dir)
				prefix := historyMessage(domain.RoleUser, "shared first event")
				base, baseManifest := seedRootReadDoc(test, dir, storage, repo, prefix, historyMessage(domain.RoleAssistant, strings.Repeat("base tail ", 70000)))
				target, targetManifest := seedRootReadDoc(test, dir, storage, repo, prefix, historyMessage(domain.RoleAssistant, strings.Repeat("target tail ", 70000)))
				service := NewService(storage, storage, nil, nil, nil)
				if _, err := service.ReadDocEvents(ctx, repo, target.Hash, base.Hash, 0, 1); err != nil {
					test.Fatal(err)
				}
				manifest := targetManifest
				if source == "base" {
					manifest = baseManifest
				}
				path := rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[len(manifest.Chunks)-1].Hash)
				want := domain.ErrIntegrity
				if mutation == "missing" {
					if err := os.Remove(path); err != nil {
						test.Fatal(err)
					}
					want = domain.ErrNotFound
				} else if err := os.WriteFile(path, []byte("altered unread tail"), 0600); err != nil {
					test.Fatal(err)
				}
				for _, offset := range []int{0, 2} {
					if _, err := service.ReadDocEvents(ctx, repo, target.Hash, base.Hash, offset, 1); !errors.Is(err, want) {
						test.Fatalf("unread %s %s accepted at offset %d: %v", source, mutation, offset, err)
					}
				}
			})
		}
	}
}

func rootReadCostFixture(tb testing.TB, payloadBytes int) (*store.FSStore, domain.ContentHash, domain.ContentHash, domain.ContentHash, int64) {
	tb.Helper()
	dir, repo := tb.TempDir(), h('1')
	storage := store.NewFSStore(dir)
	text := strings.Repeat("synthetic root page ", 200)
	events := make([]domain.CIREvent, payloadBytes/len(text))
	for index := range events {
		events[index] = historyMessage(domain.RoleUser, text)
	}
	base, _ := seedRootReadDoc(tb, dir, storage, repo, events...)
	events = append(events, historyMessage(domain.RoleAssistant, "new answer"))
	target, manifest := seedRootReadDoc(tb, dir, storage, repo, events...)
	return storage, repo, base.Hash, target.Hash, manifest.StreamBytes
}

func BenchmarkRootEventPageWorkingSet(bench *testing.B) {
	for _, sizeMiB := range []int{8, 32, 64} {
		bench.Run(fmt.Sprintf("%dMiB", sizeMiB), func(bench *testing.B) {
			storage, repo, base, target, streamBytes := rootReadCostFixture(bench, sizeMiB<<20)
			reader := &rootReadCostStore{FSStore: storage, measure: true}
			service := NewService(storage, reader, nil, nil, nil)
			runtime.GC()
			var baseline runtime.MemStats
			runtime.ReadMemStats(&baseline)
			bench.ReportAllocs()
			bench.SetBytes(streamBytes * 2)
			bench.ResetTimer()
			for iteration := 0; iteration < bench.N; iteration++ {
				page, err := service.ReadDocEvents(systemTestContext(), repo, target, base, -1, 1)
				if err != nil || len(page.Events) != 1 || page.Next != -1 {
					bench.Fatal("first changed event", err)
				}
			}
			bench.StopTimer()
			bench.ReportMetric(float64(streamBytes), "doc-B")
			if reader.peakLive > baseline.HeapAlloc {
				bench.ReportMetric(float64(reader.peakLive-baseline.HeapAlloc), "proof-live-B")
			}
			if reader.peakObserved > baseline.HeapAlloc {
				bench.ReportMetric(float64(reader.peakObserved-baseline.HeapAlloc), "sampled-heap-B")
			}
		})
	}
}
