package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyProofStreamRejectsRewrittenPhysicalSpool(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	want, plan := legacyProofStreamFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	stored, _ := json.Marshal(plan.Manifest)
	first := plan.Manifest.Chunks[0]
	corrupt := bytes.Clone(plan.Bodies[first])
	corrupt[0] ^= 1
	var cache docProofCache
	reads := 0
	proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		reads++
		if reads == 2 {
			paths, err := filepath.Glob(filepath.Join(directory, "cxthub-legacy-doc-*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("missing physical spool: %v %v", paths, err)
			}
			file, err := os.OpenFile(paths[0], os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt(plan.Bodies[first], 0)
			closeErr := file.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				t.Fatal(err)
			}
		}
		if hash == first {
			return corrupt, nil
		}
		return plan.Bodies[hash], nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() || len(cache.proofs) != 0 {
		t.Fatalf("rewritten captured bytes minted a proof: %v %v", proof, err)
	}
	proof, err = cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		if hash == first {
			return corrupt, nil
		}
		return plan.Bodies[hash], nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() || len(cache.proofs) != 0 {
		t.Fatalf("spool rewrite poisoned later cache lookup: %v %v", proof, err)
	}
	remaining, err := os.ReadDir(directory)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("integrity failure retained bytes: %v %v", remaining, err)
	}
}

func TestLegacyProofStreamIsolatesReaderManifest(t *testing.T) {
	for _, field := range []string{"envelope", "chunks"} {
		for _, timing := range []string{"factory", "load"} {
			t.Run(field+"/"+timing, func(t *testing.T) {
				document := domain.CIRDocument{Envelope: domain.CIREnvelope{SourceModels: []string{"AAA"}}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "original"}}}}}
				if field == "chunks" {
					document.Events[0].Blocks[0].Text = strings.Repeat("prefix", domain.ChunkTarget/6)
					document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "original"}}})
				}
				original, err := domain.CanonicalBytes(document)
				if err != nil {
					t.Fatal(err)
				}
				changed := bytes.Replace(original, []byte(`"AAA"`), []byte(`"BBB"`), 1)
				if field == "chunks" {
					changed = bytes.Replace(original, []byte("original"), []byte("modified"), 1)
				}
				plan, _ := domain.PlanDocChunks(original)
				changedPlan, _ := domain.PlanDocChunks(changed)
				if field == "chunks" && (len(plan.Manifest.Chunks) < 2 || plan.Manifest.Chunks[0] != changedPlan.Manifest.Chunks[0]) {
					t.Fatal("chunk mutation fixture needs a shared first chunk")
				}
				stored, _ := json.Marshal(plan.Manifest)
				repo, want := domain.HashContent([]byte(t.Name())), domain.HashContent(changed)
				var cache docProofCache
				proof, err := cache.verifyStoredWithLoader(context.Background(), repo, want, stored, func(manifest domain.DocChunkManifest) storedChunkReader {
					mutate := func() {
						if field == "envelope" {
							copy(manifest.Envelope, changedPlan.Manifest.Envelope)
						} else {
							copy(manifest.Chunks, changedPlan.Manifest.Chunks)
						}
					}
					if timing == "factory" {
						mutate()
					}
					return func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
						if timing == "load" {
							mutate()
						}
						if body, ok := changedPlan.Bodies[hash]; ok {
							return body, nil
						}
						return plan.Bodies[hash], nil
					}
				})
				if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() || len(cache.proofs) != 0 {
					t.Fatalf("reader mutated verification descriptor: %v %v", proof, err)
				}
				proof, err = cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
					return plan.Bodies[hash], nil
				})
				if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() || len(cache.proofs) != 0 {
					t.Fatalf("reader mutation poisoned later proof: %v %v", proof, err)
				}
			})
		}
	}
}

func TestLegacyProofStreamPinsStoredDescriptorBeforeReaderFactory(t *testing.T) {
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{SourceModels: []string{"AAA"}}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0}}}
	canonical, err := domain.CanonicalBytes(document)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := domain.PlanDocChunks(canonical)
	stored, _ := json.Marshal(plan.Manifest)
	original := bytes.Clone(stored)
	changed := bytes.Replace(stored, []byte(`"AAA"`), []byte(`"BBB"`), 1)
	want, repo := domain.HashContent(canonical), domain.HashContent([]byte(t.Name()))
	var cache docProofCache
	proof, err := cache.verifyStoredWithLoader(context.Background(), repo, want, stored, func(domain.DocChunkManifest) storedChunkReader {
		copy(stored, changed)
		return func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			return plan.Bodies[hash], nil
		}
	})
	if err != nil || proof.Hash() != want {
		t.Fatalf("captured descriptor changed: %v %v", proof, err)
	}
	if _, ok := cache.get(legacyProofPhysicalKey(repo, want, original, plan.Manifest, plan.Bodies)); !ok {
		t.Fatal("proof is not bound to original descriptor bytes")
	}
	proof, err = cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return plan.Bodies[hash], nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		t.Fatalf("original proof accepted mutated stored descriptor: %v %v", proof, err)
	}
}
