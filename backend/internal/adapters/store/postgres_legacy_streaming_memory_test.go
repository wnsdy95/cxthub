//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyStreamingPGMemoryQualification(t *testing.T) {
	if os.Getenv("CXT_LEGACY_MEMORY_QUALIFICATION") != "1" {
		t.Skip("opt-in 257 MiB PostgreSQL publication qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	storage, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	const eventCount = 4104
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2}
	hasher := sha256.New()
	buffer := make([]byte, 0, domain.ChunkTarget)
	streamBytes := 0
	flush := func() {
		hash := domain.HashContent(buffer)
		manifest.Chunks = append(manifest.Chunks, hash)
		if _, _, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: buffer}); err != nil {
			t.Fatal(err)
		}
		buffer = buffer[:0]
	}
	appendStream := func(body []byte) {
		hasher.Write(body)
		streamBytes += len(body)
		for len(body) > 0 {
			length := min(len(body), domain.ChunkTarget-len(buffer))
			buffer = append(buffer, body[:length]...)
			body = body[length:]
			if len(buffer) == domain.ChunkTarget {
				flush()
			}
		}
	}
	for ordinal := 0; ordinal < eventCount; ordinal++ {
		text := fmt.Sprintf("%s event %d ", repo, ordinal) + strings.Repeat("aBcD", 16384)
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
			t.Fatal("fixture not chunked")
		}
		manifest.Envelope = plan.Manifest.Envelope
		if ordinal == 0 {
			hasher.Write([]byte(`{"envelope":`))
			hasher.Write(manifest.Envelope)
			hasher.Write([]byte(`,"events":[`))
		} else {
			appendStream([]byte(","))
		}
		for _, hash := range plan.Order {
			appendStream(plan.Bodies[hash])
		}
	}
	if len(buffer) > 0 {
		flush()
	}
	hasher.Write([]byte(`]}`))
	hash := domain.ContentHash("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	job, err := domain.NewDocFinalizationJob(repo, hash, manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = storage.ClaimDocJob(ctx, repo, time.Now().UTC(), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pressure := make([]byte, 64<<20)
	for index := range pressure {
		pressure[index] = byte(index)
	}
	var peak atomic.Uint64
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				if current := stats.HeapAlloc; current > peak.Load() {
					peak.Store(current)
				}
			}
		}
	}()
	defer func() { close(stop); <-joined }()
	started := time.Now()
	report := func(phase string) {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		t.Logf("phase=%s heap=%d sys=%d peak_heap=%d", phase, stats.HeapAlloc, stats.Sys, peak.Load())
	}
	report("prepare-start")
	prepared, err := storage.PrepareLegacyDocJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	report("prepared")
	publication := prepared.(*pgLegacyStreamingPublication)
	if err := publication.Stage(ctx, job, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	report("staged")
	if err := publication.Complete(ctx, job, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	report("completed")
	index, err := storage.DocReadIndex(ctx, repo, hash)
	if err != nil || len(index.Events) != eventCount {
		t.Fatal("published index incomplete", err)
	}
	proof, err := storage.VerifyStoredDoc(ctx, repo, hash)
	if err != nil || !proof.Valid() || proof.Hash() != hash {
		t.Fatal("reference verification failed", err)
	}
	report("reference-verified")
	runtime.KeepAlive(pressure)
	t.Logf("transcript_bytes=%d chunks=%d events=%d elapsed=%s peak_heap=%d concurrent_pressure=%d", streamBytes, len(manifest.Chunks), eventCount, time.Since(started), peak.Load(), len(pressure))
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "VmRSS:") {
				t.Log(line)
			}
		}
	}
	if peak.Load() > 320<<20 {
		t.Fatalf("peak live heap %d exceeds 320 MiB with 64 MiB concurrent pressure", peak.Load())
	}
}
