package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestStoredDocProofRechecksCurrentBytesOwnershipAndExistence(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	doc, v := verifiedDocFixture(t)
	if _, err := s.PutVerifiedDoc(ctx, repo, v); err != nil {
		t.Fatal(err)
	}
	cold := NewFSStore(s.dataDir)
	for i := 0; i < 2; i++ {
		p, err := cold.VerifyStoredDoc(ctx, repo, doc.Hash)
		if err != nil || !p.Matches(domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash}) {
			t.Fatalf("proof %d: %v %v", i, p, err)
		}
	}
	if len(cold.docProofs.proofs) != 1 {
		t.Fatal("proof was not retained")
	}
	if _, err := cold.VerifyStoredDoc(ctx, domain.HashContent([]byte("foreign")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof granted foreign ownership", err)
	}
	// Replace the path at the same expected address after warming the cache.
	if err := writeAtomic(s.docPath(repo, doc.Hash), []byte(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := cold.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("proof hid changed bytes", err)
	}
	if err := os.Remove(s.docPath(repo, doc.Hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := cold.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof hid removed body", err)
	}
}
func TestStoredDocProofChecksChunksAfterWarmup(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("capture", 500000)}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := s.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	man, err := s.GetDocManifest(ctx, repo, doc.Hash)
	if err != nil || len(man.Chunks) == 0 {
		t.Fatalf("chunk fixture: %v %v", man, err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	path := s.chunkPath(repo, man.Chunks[0])
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, docCompress([]byte("tampered"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("proof hid corrupt chunk", err)
	}
	if err := writeAtomic(path, prior); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof hid missing chunk", err)
	}
}
func TestStoredDocProofLegacyAndSchemaValidation(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	doc, v := verifiedDocFixture(t)
	legacy, err := json.MarshalIndent(doc.CIR, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(s.docPath(repo, doc.Hash), legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil {
		t.Fatal("legacy canonical identity rejected", err)
	}
	if domain.HashContent(legacy) == v.Hash() {
		t.Fatal("fixture is not legacy representation")
	}
	for _, raw := range [][]byte{
		[]byte(`{"envelope":{"cir_version":"999"},"events":[]}`),
		[]byte(`{"envelope":{"cir_version":"1"},"events":[{"kind":"compaction","seq":1}]}`),
	} {
		hash := domain.HashContent(raw)
		if err := writeAtomic(s.docPath(repo, hash), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyStoredDoc(ctx, repo, hash); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("unsupported schema accepted: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.VerifyStoredDoc(cancelled, repo, doc.Hash); !errors.Is(err, context.Canceled) {
		t.Fatal("warm proof ignored cancellation", err)
	}
}
func TestDocProofCacheBoundedAndConcurrent(t *testing.T) {
	_, v := verifiedDocFixture(t)
	var c docProofCache
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < maxDocProofs; n++ {
				repo := domain.HashContent([]byte{byte(worker), byte(n >> 8), byte(n)})
				key := docProofKey{repo: repo, expected: v.Hash(), representation: v.Hash()}
				c.put(key, v.Reference())
				c.get(key)
			}
		}(i)
	}
	wg.Wait()
	if len(c.proofs) != maxDocProofs || len(c.order) != maxDocProofs {
		t.Fatalf("unbounded cache %d/%d", len(c.proofs), len(c.order))
	}
}
func BenchmarkStoredDocVerification(b *testing.B) {
	ctx := context.Background()
	s := NewFSStore(b.TempDir())
	repo := domain.HashContent([]byte("benchmark"))
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{}}}
	for i := 0; i < 128; i++ {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: i, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("bounded capture body ", 3200)}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		b.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := s.PutDoc(ctx, repo, doc); err != nil {
		b.Fatal(err)
	}
	b.Run("legacy-full-validation", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			d, err := s.GetDoc(ctx, repo, doc.Hash)
			if err != nil {
				b.Fatal(err)
			}
			if err := domain.ValidateSessionDocHash(d); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cold-proof", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cold := NewFSStore(s.dataDir)
			if _, err := cold.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("warm-proof", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil {
				b.Fatal(err)
			}
		}
	})
}
