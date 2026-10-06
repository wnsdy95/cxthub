package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func storedChunkProofFixture(t testing.TB) (domain.ContentHash, []byte, domain.DocChunkPlan) {
	t.Helper()
	doc := domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", 3*domain.ChunkTarget)}}}}}
	raw, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Manifest.Chunks) <= len(plan.Bodies) {
		t.Fatal("fixture must have duplicate chunk occurrences")
	}
	return domain.HashContent(raw), raw, plan
}

func TestStoredChunkProofRepackingAndDescriptorChanges(t *testing.T) {
	ctx := context.Background()
	hash, canonical, plan := storedChunkProofFixture(t)
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	for _, legacy := range []bool{false, true} {
		if legacy {
			var ok bool
			plan, ok = domain.PlanDocChunksV1(canonical)
			if !ok {
				t.Fatal("v1 fixture")
			}
		}
		if _, err := s.putDocChunkPlan(repo, hash, plan); err != nil {
			t.Fatal(err)
		}
		for _, compressed := range []bool{true, false, true} {
			for ch, raw := range plan.Bodies {
				if compressed {
					raw = docCompress(raw)
				}
				if err := writeAtomic(s.chunkPath(repo, ch), raw); err != nil {
					t.Fatal(err)
				}
			}
			manifest, _ := json.Marshal(plan.Manifest)
			if compressed {
				manifest = docCompress(manifest)
			}
			if err := writeAtomic(s.docPath(repo, hash), manifest); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				p, err := s.VerifyStoredDoc(ctx, repo, hash)
				if err != nil || p.Hash() != hash {
					t.Fatalf("repacked %s compressed=%v: %v %v", plan.Manifest.Format, compressed, p, err)
				}
			}
		}
		// The same owned bodies cannot validate changed envelope/order/count.
		for _, change := range []func(*domain.DocChunkManifest){
			func(m *domain.DocChunkManifest) { m.Envelope = json.RawMessage(`{"source_provider":"changed"}`) },
			func(m *domain.DocChunkManifest) { slices.Reverse(m.Chunks) },
			func(m *domain.DocChunkManifest) { m.Chunks = append(m.Chunks, m.Chunks[0]) },
			func(m *domain.DocChunkManifest) { m.Chunks[0] = "../invalid" },
		} {
			m := plan.Manifest
			m.Chunks = slices.Clone(m.Chunks)
			change(&m)
			if slices.Equal(m.Chunks, plan.Manifest.Chunks) && bytes.Equal(m.Envelope, plan.Manifest.Envelope) {
				continue // A one-chunk legacy manifest has no distinct reverse.
			}
			manifest, _ := json.Marshal(m)
			if err := writeAtomic(s.docPath(repo, hash), manifest); err != nil {
				t.Fatal(err)
			}
			if _, err := s.VerifyStoredDoc(ctx, repo, hash); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("changed descriptor reused a warm proof", err)
			}
		}
	}
}

func TestStoredChunkProofRetainsCapturedBytesAndCancellation(t *testing.T) {
	ctx := context.Background()
	hash, _, plan := storedChunkProofFixture(t)
	repo := domain.HashContent([]byte(t.Name()))
	manifest, _ := json.Marshal(plan.Manifest)
	var cache docProofCache
	reads := map[domain.ContentHash]int{}
	first := plan.Manifest.Chunks[0]
	// A storage change after the first read must not lead to rereading valid
	// bytes and caching a proof for the corrupt bytes fingerprinted earlier.
	changed := false
	read := func(_ context.Context, ch domain.ContentHash) ([]byte, error) {
		reads[ch]++
		if ch == first && !changed {
			changed = true
			return []byte("corrupt prior representation"), nil
		}
		return slices.Clone(plan.Bodies[ch]), nil
	}
	if _, err := cache.verifyStored(ctx, repo, hash, manifest, read); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cold verification did not use captured bytes", err)
	}
	if len(cache.proofs) != 0 {
		t.Fatal("failed verification cached a proof")
	}
	for _, n := range reads {
		if n != 1 {
			t.Fatal("chunk reread during one verification", reads)
		}
	}
	clear(reads)
	for i := 0; i < 2; i++ {
		if p, err := cache.verifyStored(ctx, repo, hash, manifest, read); err != nil || p.Hash() != hash {
			t.Fatal("restored bytes", p, err)
		}
	}
	for _, n := range reads {
		if n != 2 {
			t.Fatal("warm cache skipped current byte reads", reads)
		}
	}
	changed = false
	if _, err := cache.verifyStored(ctx, repo, hash, manifest, read); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("ABA corruption reused a valid proof", err)
	}
	for _, warm := range []bool{false, true} {
		c := &cache
		if !warm {
			c = &docProofCache{}
		}
		cancelled, cancel := context.WithCancel(ctx)
		n := 0
		p, err := c.verifyStored(cancelled, repo, hash, manifest, func(_ context.Context, ch domain.ContentHash) ([]byte, error) {
			n++
			if n == len(plan.Bodies) {
				cancel()
			}
			return slices.Clone(plan.Bodies[ch]), nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) || p.Valid() {
			t.Fatalf("cancellation after last read warm=%v: %v %v", warm, p, err)
		}
	}
}

func TestStoredChunkProofDoesNotAcceptUnvalidatedCanonicalHash(t *testing.T) {
	// A matching byte hash alone does not establish a supported CIR schema.
	raw := []byte(`{"envelope":{"cir_version":"999"},"events":[{"kind":"message","seq":1}]}`)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("invalid-schema chunk fixture")
	}
	manifest, _ := json.Marshal(plan.Manifest)
	var cache docProofCache
	_, err := cache.verifyStored(context.Background(), domain.HashContent([]byte(t.Name())), domain.HashContent(raw), manifest, func(_ context.Context, ch domain.ContentHash) ([]byte, error) {
		return slices.Clone(plan.Bodies[ch]), nil
	})
	if !errors.Is(err, domain.ErrIntegrity) || len(cache.proofs) != 0 {
		t.Fatal("chunk hash bypassed schema validation", err)
	}
}
