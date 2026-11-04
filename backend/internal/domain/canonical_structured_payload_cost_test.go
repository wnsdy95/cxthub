package domain

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func canonicalStructuredPayloadFixture(test testing.TB, count int, object bool) []byte {
	test.Helper()
	opening, closing := byte('['), byte(']')
	if object {
		opening, closing = '{', '}'
	}
	payload := make([]byte, 0, count*64)
	payload = append(payload, opening)
	for ordinal := 0; ordinal < count; ordinal++ {
		if ordinal != 0 {
			payload = append(payload, ',')
		}
		if object {
			payload = fmt.Appendf(payload, `"key%08d":`, ordinal)
		}
		payload = append(payload, `{"flag":false,"number":`...)
		payload = strconv.AppendInt(payload, int64(ordinal), 10)
		payload = append(payload, `,"text":"synthetic"}`...)
	}
	payload = append(payload, closing)
	event := `{"call_id":"tool","kind":"tool_result","output":` + string(payload) + `,"provider_metadata":{"create_time":123.5},"seq":1}`
	return giantCanonicalDocument(test, event)
}

func BenchmarkCanonicalStructuredPayloadColdVerification(benchmark *testing.B) {
	for _, count := range []int{8192, 65536, 262144} {
		for _, object := range []bool{false, true} {
			benchmark.Run(fmt.Sprintf("items=%d/object=%t", count, object), func(benchmark *testing.B) {
				raw := canonicalStructuredPayloadFixture(benchmark, count, object)
				want := HashContent(raw)
				runtime.GC()
				runtime.GC()
				benchmark.ReportAllocs()
				benchmark.SetBytes(int64(len(raw)))
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					var verifier CanonicalDocVerifier
					proof, err := verifier.Verify(context.Background(), want, raw)
					if err != nil || !proof.Valid() || proof.Hash() != want {
						benchmark.Fatal(err)
					}
					runtime.KeepAlive(proof)
				}
			})
		}
	}
}

func TestCanonicalStructuredPayloadColdVerificationCost(test *testing.T) {
	if os.Getenv("CXT_STRUCTURED_PAYLOAD_QUALIFICATION") != "1" {
		test.Skip("opt-in complete structured-payload cold verification interval")
	}
	for _, count := range []int{65536, 262144} {
		test.Run(strconv.Itoa(count), func(test *testing.T) {
			raw := canonicalStructuredPayloadFixture(test, count, true)
			want := HashContent(raw)
			runtime.GC()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			var peak atomic.Uint64
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
			var verifier CanonicalDocVerifier
			proof, err := verifier.Verify(context.Background(), want, raw)
			elapsed := time.Since(started)
			observe()
			close(stop)
			<-joined
			if err != nil || !proof.Valid() || proof.Hash() != want {
				test.Fatal(err)
			}
			test.Logf("items=%d source_bytes=%d elapsed_ns=%d base_heap=%d observed_peak_heap=%d sample_interval_ns=%d", count, len(raw), elapsed.Nanoseconds(), before.HeapAlloc, peak.Load(), time.Millisecond.Nanoseconds())
			runtime.KeepAlive(raw)
			runtime.KeepAlive(proof)
		})
	}
}
