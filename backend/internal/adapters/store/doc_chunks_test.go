package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// chunkBigDoc creates an n-event CIR from the deterministic pseudorandom body of a big document (compression resistance — storage capacity validation).
func chunkBigDoc(n int) domain.CIRDocument {
	doc := domain.CIRDocument{}
	doc.Envelope.CIRVersion = "1"
	doc.Envelope.SourceProvider = "claude"
	for i := 0; i < n; i++ {
		var b strings.Builder
		seed := []byte{byte(i), byte(i >> 8)}
		for b.Len() < 40<<10 {
			sum := sha256.Sum256(append(seed, byte(b.Len()), byte(b.Len()>>8), byte(b.Len()>>16)))
			b.WriteString(hex.EncodeToString(sum[:]))
		}
		doc.Events = append(doc.Events, domain.CIREvent{
			Kind: domain.EventMessage, Role: "user", Seq: i,
			Blocks: []domain.ContentBlock{{Type: "text", Text: b.String()}},
		})
	}
	return doc
}

// TestFSDocChunkRoundtripAndRepack fixes the server FS store chunk CAS contract:
// put→get lossless roundtrip, prefix chunk dedup for growing docs, legacy monolithic hash repacking.
func TestFSDocChunkRoundtripAndRepack(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('0')

	mkDoc := func(n int) domain.SessionDoc {
		cir := chunkBigDoc(n)
		cb, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		return domain.SessionDoc{Hash: domain.HashContent(cb), CIR: cir}
	}
	d1 := mkDoc(40)
	if _, err := st.PutDoc(ctx, repo, d1); err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	got, err := st.GetDoc(ctx, repo, d1.Hash)
	if err != nil {
		t.Fatalf("GetDoc(chunked): %v", err)
	}
	if len(got.CIR.Events) != 40 {
		t.Fatalf("Roundtrip mismatch: %d events", len(got.CIR.Events))
	}
	if err := domain.ValidateSessionDocHash(got); err != nil {
		t.Fatalf("Integrity check: %v", err)
	}
	chunksDir := filepath.Join(st.repoDir(repo), "objects", "chunks")
	before, _ := os.ReadDir(chunksDir)
	if len(before) < 2 {
		t.Fatalf("Multi-chunk expected: %d", len(before))
	}
	// Growing doc: prefix chunk reuse.
	d2 := mkDoc(55)
	if _, err := st.PutDoc(ctx, repo, d2); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range before {
		names[e.Name()] = true
	}
	after, _ := os.ReadDir(chunksDir)
	shared := 0
	for _, e := range after {
		if names[e.Name()] {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("Prefix chunk dedup failed")
	}

	// Directly embed legacy monolith and repack → convert to same hash as chunk.
	d3 := mkDoc(70)
	cb, _ := domain.CanonicalBytes(d3.CIR)
	legacy := st.docPath(repo, d3.Hash)
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, docCompress(cb), 0o644); err != nil {
		t.Fatal(err)
	}
	n, _, err := st.RepackDocs()
	if err != nil {
		t.Fatalf("RepackDocs: %v", err)
	}
	if n != 1 {
		t.Fatalf("Conversion count: got %d want 1", n)
	}
	if got3, err := st.GetDoc(ctx, repo, d3.Hash); err != nil || len(got3.CIR.Events) != 70 {
		t.Fatalf("Repack error after GetDoc: %v", err)
	}
	if n2, _, _ := st.RepackDocs(); n2 != 0 {
		t.Fatalf("Repack idempotence violation: %d", n2)
	}

	// Non-canonical bytes (legacy from canonical sorting before server records — empirically 162/214): raw hash is
	// inconsistent but parsing→canonical recalculation matches → must be normalized and repacked as canonical.
	d4 := mkDoc(45)
	nonCanonical, err := json.Marshal(d4.CIR) // struct field order serialization ≠ canonical (key sorting)
	if err != nil {
		t.Fatal(err)
	}
	if domain.HashContent(nonCanonical) == d4.Hash {
		t.Fatal("Test precondition failure: struct serialization is not canonical")
	}
	p4 := st.docPath(repo, d4.Hash)
	if err := os.WriteFile(p4, docCompress(nonCanonical), 0o644); err != nil {
		t.Fatal(err)
	}
	n3, _, err := st.RepackDocs()
	if err != nil || n3 != 1 {
		t.Fatalf("Non-canonical legacy repack: n=%d err=%v", n3, err)
	}
	if got4, err := st.GetDoc(ctx, repo, d4.Hash); err != nil || len(got4.CIR.Events) != 45 {
		t.Fatalf("Post-normalization repacking GetDoc: %v", err)
	}
}
