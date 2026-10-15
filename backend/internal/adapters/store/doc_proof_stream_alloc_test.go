package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyProofStreamAllocationQualification(t *testing.T) {
	if os.Getenv("CXT_LEGACY_PROOF_ALLOCATION") != "1" {
		t.Skip("opt-in large legacy reference allocation diagnosis")
	}
	ctx := context.Background()
	input, err := newLegacyDocSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2}
	const count = 4104
	for ordinal := range count {
		text := fmt.Sprintf("event %d ", ordinal) + strings.Repeat("aBcD", 16384)
		event := domain.CIREvent{Seq: ordinal, Kind: domain.EventToolResult, CallID: fmt.Sprint(ordinal), Output: text}
		if ordinal%8 == 0 {
			event = domain.CIREvent{Seq: ordinal, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
		}
		canonical, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2"}, Events: []domain.CIREvent{event}})
		if err != nil {
			t.Fatal(err)
		}
		plan, ok := domain.PlanDocChunks(canonical)
		if !ok {
			t.Fatal("fixture must be chunked")
		}
		manifest.Envelope = plan.Manifest.Envelope
		if ordinal > 0 {
			if err := input.Append(ctx, []byte(",")); err != nil {
				t.Fatal(err)
			}
		}
		for _, hash := range plan.Order {
			if err := input.Append(ctx, plan.Bodies[hash]); err != nil {
				t.Fatal(err)
			}
		}
	}
	directory := t.TempDir()
	fingerprint := sha256.New()
	fingerprint.Write([]byte(`{"envelope":`))
	fingerprint.Write(manifest.Envelope)
	fingerprint.Write([]byte(`,"events":[`))
	for offset := 0; offset < input.size; offset += domain.ChunkTarget {
		body, err := input.Read(ctx, offset, min(domain.ChunkTarget, input.size-offset))
		if err != nil {
			t.Fatal(err)
		}
		fingerprint.Write(body)
		hash := domain.HashContent(body)
		manifest.Chunks = append(manifest.Chunks, hash)
		if err := os.WriteFile(filepath.Join(directory, string(hash)[7:]), docCompress(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint.Write([]byte(`]}`))
	want := domain.ContentHash("sha256:" + hex.EncodeToString(fingerprint.Sum(nil)))
	stored, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	pressure := make([]byte, 64<<20)
	for index := range pressure {
		pressure[index] = byte(index)
	}
	var cache docProofCache
	repo := domain.HashContent([]byte(t.Name()))
	for _, phase := range []string{"cold", "semantic-warm", "physical-warm"} {
		if phase == "semantic-warm" {
			cache.proofs, cache.order = nil, nil
		}
		var before, after, retained runtime.MemStats
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		peak.Store(before.HeapAlloc)
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					var current runtime.MemStats
					runtime.ReadMemStats(&current)
					if current.HeapAlloc > peak.Load() {
						peak.Store(current.HeapAlloc)
					}
				}
			}
		}()
		started := time.Now()
		reads := 0
		proof, err := cache.verifyStored(ctx, repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			reads++
			return os.ReadFile(filepath.Join(directory, string(hash)[7:]))
		})
		close(stop)
		<-done
		runtime.ReadMemStats(&after)
		runtime.GC()
		runtime.ReadMemStats(&retained)
		runtime.KeepAlive(&cache)
		runtime.KeepAlive(pressure)
		if err != nil || proof.Hash() != want || reads != len(manifest.Chunks) {
			t.Fatalf("%s failed: %v %v reads=%d", phase, proof, err, reads)
		}
		t.Logf("phase=%s bytes=%d chunks=%d elapsed=%s before_heap=%d peak_heap=%d after_heap=%d retained_heap=%d next_gc=%d allocated=%d gc_count=%d",
			phase, input.size, reads, time.Since(started), before.HeapAlloc, peak.Load(), after.HeapAlloc, retained.HeapAlloc, after.NextGC, after.TotalAlloc-before.TotalAlloc, after.NumGC-before.NumGC)
	}
}
