package domain

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

var legacyDerivedMetadataBenchmark VerifiedLegacyDerivedRangeMetadata

func BenchmarkLegacyDerivedMetadataPreparation(benchmark *testing.B) {
	for _, count := range []int{257, 4097, 16385} {
		benchmark.Run(fmt.Sprint(count), func(benchmark *testing.B) {
			proof, _ := legacyDocRangeGenerationFixture(benchmark, count)
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
				if err != nil || !metadata.Valid() {
					benchmark.Fatal(err)
				}
				legacyDerivedMetadataBenchmark = metadata
			}
		})
	}
}

func TestLegacyDerivedMetadataPreparationCost(test *testing.T) {
	if os.Getenv("CXT_LEGACY_METADATA_PREPARATION_QUALIFICATION") != "1" {
		test.Skip("opt-in complete metadata-preparation interval measurement")
	}
	for _, count := range []int{4097, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, _ := legacyDocRangeGenerationFixture(test, count)
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			var peak atomic.Uint64
			peak.Store(before.HeapAlloc)
			observe := func() uint64 {
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				for current := peak.Load(); stats.HeapAlloc > current; current = peak.Load() {
					if peak.CompareAndSwap(current, stats.HeapAlloc) {
						break
					}
				}
				return stats.HeapAlloc
			}
			stop, joined, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(joined)
				observe()
				close(ready)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						observe()
					}
				}
			}()
			<-ready
			started := time.Now()
			var firstSave time.Duration
			var firstSaveHeap uint64
			objects, savedBytes := 0, 0
			metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
				if objects == 0 {
					firstSave = time.Since(started)
					firstSaveHeap = observe()
				}
				objects++
				savedBytes += len(object.Bytes)
				return nil
			})
			elapsed := time.Since(started)
			observe()
			close(stop)
			<-joined
			if err != nil || !metadata.Valid() || objects != metadata.ObjectCount() {
				test.Fatalf("metadata measurement failed: %v", err)
			}
			test.Logf("events=%d objects=%d bytes=%d elapsed_ns=%d first_save_ns=%d base_heap=%d first_save_heap=%d observed_peak_heap=%d sample_interval_ns=%d", count, objects, savedBytes,
				elapsed.Nanoseconds(), firstSave.Nanoseconds(), before.HeapAlloc, firstSaveHeap, peak.Load(), time.Millisecond.Nanoseconds())
			runtime.KeepAlive(proof)
			runtime.KeepAlive(metadata)
		})
	}
}
