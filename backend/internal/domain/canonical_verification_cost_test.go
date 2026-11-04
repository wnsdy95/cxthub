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

func TestCanonicalGiantStringColdVerificationCost(test *testing.T) {
	if os.Getenv("CXT_CANONICAL_VERIFICATION_QUALIFICATION") != "1" {
		test.Skip("opt-in complete cold verification interval measurement")
	}
	for _, size := range []int{8 << 20, 24 << 20} {
		raw := giantCanonicalDocument(test, `{"call_id":"tool","kind":"tool_result","output":`+giantCanonicalString(test, string(make([]byte, size/6)))+`,"seq":1}`)
		want := HashContent(raw)
		for _, variant := range []string{"archived", "projected"} {
			test.Run(fmt.Sprintf("bytes=%d/%s", len(raw), variant), func(test *testing.T) {
				runtime.GC()
				runtime.GC()
				var before runtime.MemStats
				var peak atomic.Uint64
				runtime.ReadMemStats(&before)
				peak.Store(before.HeapAlloc)
				observe := func() {
					var stats runtime.MemStats
					runtime.ReadMemStats(&stats)
					for current := peak.Load(); stats.HeapAlloc > current; current = peak.Load() {
						if peak.CompareAndSwap(current, stats.HeapAlloc) {
							break
						}
					}
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
				var proof VerifiedSessionDoc
				var err error
				if variant == "archived" {
					proof, err = archivedColdCanonicalVerify(context.Background(), want, raw)
				} else {
					var verifier CanonicalDocVerifier
					proof, err = verifier.Verify(context.Background(), want, raw)
				}
				elapsed := time.Since(started)
				observe()
				close(stop)
				<-joined
				if err != nil || !proof.Valid() || proof.Hash() != want {
					test.Fatal("cold verification failed", err)
				}
				test.Logf("source_bytes=%d variant=%s elapsed_ns=%d base_heap=%d observed_peak_heap=%d sample_interval_ns=%d", len(raw), variant,
					elapsed.Nanoseconds(), before.HeapAlloc, peak.Load(), time.Millisecond.Nanoseconds())
				runtime.KeepAlive(raw)
				runtime.KeepAlive(proof)
			})
		}
	}
}
