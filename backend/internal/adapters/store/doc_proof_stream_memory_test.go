package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyProofStreamRetainedBytes(t *testing.T) {
	const worker = "CXT_TEST_LEGACY_PROOF_MEMORY_WORKER"
	if os.Getenv(worker) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestLegacyProofStreamRetainedBytes$", "-test.v")
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "GOMEMLIMIT=") && !strings.HasPrefix(variable, "GOMAXPROCS=") && !strings.HasPrefix(variable, worker+"=") {
				command.Env = append(command.Env, variable)
			}
		}
		command.Env = append(command.Env, worker+"=1", "GOMEMLIMIT=128MiB", "GOMAXPROCS=1")
		output, err := command.CombinedOutput()
		t.Logf("isolated retention check:\n%s", output)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	directory := t.TempDir()
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2}
	complete := sha256.New()
	for ordinal := range 128 {
		doc := domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: ordinal,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("MiXeD body ", 25000)}}}}}
		canonical, err := domain.CanonicalBytes(doc)
		if err != nil {
			t.Fatal(err)
		}
		plan, ok := domain.PlanDocChunks(canonical)
		if !ok || len(plan.Manifest.Chunks) != 1 {
			t.Fatal("memory fixture needs one event per stored chunk")
		}
		body := plan.Bodies[plan.Manifest.Chunks[0]]
		if ordinal == 0 {
			manifest.Envelope = bytes.Clone(plan.Manifest.Envelope)
			complete.Write([]byte(`{"envelope":`))
			complete.Write(manifest.Envelope)
			complete.Write([]byte(`,"events":[`))
		} else {
			body = append([]byte{','}, body...)
		}
		complete.Write(body)
		hash := domain.HashContent(body)
		manifest.Chunks = append(manifest.Chunks, hash)
		if err := os.WriteFile(filepath.Join(directory, strings.TrimPrefix(string(hash), "sha256:")), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	complete.Write([]byte(`]}`))
	want := domain.ContentHash("sha256:" + hex.EncodeToString(complete.Sum(nil)))
	stored, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name()))
	var cache docProofCache
	for _, phase := range []string{"cold", "warm"} {
		runtime.GC()
		var baseline runtime.MemStats
		runtime.ReadMemStats(&baseline)
		reads := 0
		var retained uint64
		proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			reads++
			if reads == len(manifest.Chunks) {
				runtime.GC()
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				if current.HeapAlloc > baseline.HeapAlloc {
					retained = current.HeapAlloc - baseline.HeapAlloc
				}
				if retained > 8<<20 {
					t.Fatalf("%s retained %d bytes before final read; limit 8MiB", phase, retained)
				}
			}
			return os.ReadFile(filepath.Join(directory, strings.TrimPrefix(string(hash), "sha256:")))
		})
		if err != nil || proof.Hash() != want || reads != len(manifest.Chunks) {
			t.Fatalf("%s reference: %v %v reads=%d", phase, proof, err, reads)
		}
		t.Logf("phase=%s chunks=%d retained_current_bytes=%d", phase, reads, retained)
	}
}
