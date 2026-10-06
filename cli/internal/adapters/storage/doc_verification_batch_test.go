package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func BenchmarkColdDocumentBatch(b *testing.B) {
	for _, shape := range []string{"single", "cumulative", "unrelated"} {
		b.Run(shape, func(b *testing.B) {
			fixture := NewFileStore(b.TempDir())
			var ids []domain.ContentHash
			count := 1
			if shape != "single" {
				count = 8
			}
			for n := 0; n < count; n++ {
				doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", GitBranch: fmt.Sprint(n)}}
				for i := 0; i < 128+n; i++ {
					text := strings.Repeat("context ", 8192)
					if shape == "unrelated" {
						text = strings.Repeat(fmt.Sprintf("text%03d ", n), 8192)
					}
					doc.Events = append(doc.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
				}
				id, err := fixture.PutDoc(context.Background(), domain.SessionDoc{CIR: doc})
				if err != nil {
					b.Fatal(err)
				}
				ids = append(ids, id)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// A fresh process-equivalent store for EACH batch. No persistent receipts
				// or event proofs from a previous iteration may make a cold batch warm.
				store := NewFileStore(fixture.repoRoot)
				for _, id := range ids {
					if err := store.VerifyStoredDoc(context.Background(), id); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func TestDocVerificationEventReuseKeepsCurrentEvidence(t *testing.T) {
	s := verificationStore(t)
	doc := bigDoc(30)
	a, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: doc})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDoc(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	doc.Envelope.GitBranch = "other"
	b, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: doc})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDoc(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	proof := receiptFor(t, s, b)
	if proof.Proof.Doc != b || proof.Proof.Files[0].ID != b {
		t.Fatal("reused another document's receipt")
	}
	// Lose only the optimization hint, then change a current shared chunk. An
	// exact cached event from the earlier doc must never hide physical damage.
	if err := os.Remove(s.docReceiptPath(b)); err != nil {
		t.Fatal(err)
	}
	chunk := proof.Proof.Files[1]
	if err := writeAtomic(s.objectPath("chunks", chunk.ID), docCompress([]byte("corrupt"))); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDoc(context.Background(), b); err == nil {
		t.Fatal("event proof hid changed current bytes")
	}
	if _, err := os.Stat(s.docReceiptPath(b)); !os.IsNotExist(err) {
		t.Fatal("damaged document got a receipt")
	}
}

func TestDocVerificationLegacyFallbackReceiptBindsStoredBytes(t *testing.T) {
	s := verificationStore(t)
	cir := bigDoc(1)
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.HashContent(canonical)
	legacy, err := json.MarshalIndent(cir, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(s.objectPath("docs", id), legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	proof := receiptFor(t, s, id)
	if len(proof.Proof.Files) != 1 || proof.Proof.Files[0].Stored != domain.HashContent(legacy) {
		t.Fatal("receipt not bound to actual legacy representation")
	}
	fresh := NewFileStore(s.repoRoot)
	fresh.EnableDocVerificationCache(s.docProofKeyPath)
	if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
