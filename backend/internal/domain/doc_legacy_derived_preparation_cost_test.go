package domain

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

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
			elapsed := time.Since(started)
			observe()
			close(stop)
			<-joined
			test.Logf("events=%d objects=%d bytes=%d elapsed_ns=%d first_save_ns=%d base_heap=%d first_save_heap=%d observed_peak_heap=%d sample_interval_ns=%d", count, objects, savedBytes,
				elapsed.Nanoseconds(), firstSave.Nanoseconds(), before.HeapAlloc, firstSaveHeap, peak.Load(), time.Millisecond.Nanoseconds())
			runtime.KeepAlive(proof)
		})
	}
}
