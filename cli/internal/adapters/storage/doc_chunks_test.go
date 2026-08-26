package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// bigDoc creates an n-event CIR (each ~40KB — exceeding the chunk threshold).
// The body is filled with deterministic pseudorandom numbers (sha256 chain) to compress poorly —
// single-character repetition makes zstd store deltas instead of entire chunks, rendering size comparisons meaningless.
func bigDoc(n int) domain.CIRDocument {
	doc := domain.CIRDocument{}
	doc.Envelope.CIRVersion = "1"
	doc.Envelope.SourceProvider = domain.ProviderClaude
	for i := 0; i < n; i++ {
		var b strings.Builder
		seed := []byte{byte(i), byte(i >> 8)}
		for b.Len() < 40<<10 {
			sum := sha256.Sum256(append(seed, byte(b.Len()), byte(b.Len()>>8), byte(b.Len()>>16)))
			b.WriteString(hex.EncodeToString(sum[:]))
		}
		doc.Events = append(doc.Events, domain.Event{
			Kind: domain.EventMessage, Role: "user", Seq: i,
			Blocks: []domain.ContentBlock{{Type: "text", Text: b.String()}},
		})
	}
	return doc
}

func chunkFiles(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	dir := filepath.Join(root, ".cxt", "objects", "chunks")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, _ := e.Info()
		out[e.Name()] = fi.Size()
	}
	return out
}

// TestDocChunkRoundtripAndPrefixDedup fixes the core contract of chunk CAS:
// (1) put→get lossless roundtrip (hash invariant), (2) append-only growth with closed prefix chunks
// captured for deduplication, storing only deltas (full chunk re-save — 97% duplication observed — structural resolution).
func TestDocChunkRoundtripAndPrefixDedup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)

	d1 := bigDoc(40) // ~1.6MB → multiple chunks
	h1, err := st.PutDoc(ctx, domain.SessionDoc{CIR: d1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDoc(ctx, h1)
	if err != nil {
		t.Fatalf("GetDoc(chunked): %v", err)
	}
	if len(got.CIR.Events) != 40 || got.Hash != h1 {
		t.Fatalf("Roundtrip mismatch: events=%d hash=%s", len(got.CIR.Events), got.Hash)
	}
	if err := domain.ValidateSessionDocHash(got); err != nil {
		t.Fatalf("Hash integrity check failed: %v", err)
	}
	c1 := chunkFiles(t, root)
	if len(c1) < 2 {
		t.Fatalf("Multi-chunk expected, got %d", len(c1))
	}

	// Growing doc (same prefix + new events) → reusing closed chunks.
	d2 := bigDoc(55)
	h2, err := st.PutDoc(ctx, domain.SessionDoc{CIR: d2})
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h1 {
		t.Fatal("Different docs with the same hash")
	}
	c2 := chunkFiles(t, root)
	shared := 0
	for name := range c1 {
		if _, ok := c2[name]; ok {
			shared++
		}
	}
	if shared == 0 {
		t.Fatalf("Prefix chunk deduplication failed: Shared chunk 0 (c1=%d c2=%d)", len(c1), len(c2))
	}
	if got2, err := st.GetDoc(ctx, h2); err != nil || len(got2.CIR.Events) != 55 {
		t.Fatalf("Growth doc round trip failed: %v", err)
	}
}

// TestDocChunkCorruptionDetected fixes the issue where chunk corruption is detected by hash comparison.
func TestDocChunkCorruptionDetected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)
	h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: bigDoc(40)})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".cxt", "objects", "chunks")
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("No chunk")
	}
	victim := filepath.Join(dir, entries[0].Name())
	small := bigDoc(1)
	cb, _ := domain.CanonicalBytes(small)
	if err := os.WriteFile(victim, docCompress(cb), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDoc(ctx, h); err == nil {
		t.Fatal("Contaminated chunk not detected")
	}
}

// TestRepackDocsLegacyBlob ensures that legacy blob docs are converted to chunked format with the same hash
// and that GetDoc works afterward (lossless repacking — recovery path from the original 1.0GB).
func TestRepackDocsLegacyBlob(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)

	// Directly recreate legacy blob storage: docs/<hash> = zstd(canonical).
	writeLegacy := func(n int) domain.ContentHash {
		doc := bigDoc(n)
		cb, err := domain.CanonicalBytes(doc)
		if err != nil {
			t.Fatal(err)
		}
		h := domain.HashContent(cb)
		p := filepath.Join(root, ".cxt", "objects", "docs", strings.TrimPrefix(string(h), "sha256:"))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, docCompress(cb), 0o644); err != nil {
			t.Fatal(err)
		}
		return h
	}
	h1 := writeLegacy(40)
	h2 := writeLegacy(55) // same prefix — repack after chunk sharing

	sizeBefore := dirSize(t, filepath.Join(root, ".cxt", "objects"))
	n, saved, err := st.RepackDocs()
	if err != nil {
		t.Fatalf("RepackDocs: %v", err)
	}
	if n != 2 {
		t.Fatalf("conversion count: got %d want 2", n)
	}
	sizeAfter := dirSize(t, filepath.Join(root, ".cxt", "objects"))
	if sizeAfter >= sizeBefore {
		t.Fatalf("disk size did not decrease after repacking: %d → %d (saved=%d)", sizeBefore, sizeAfter, saved)
	}
	if saved != sizeBefore-sizeAfter {
		t.Fatalf("reported reclaimed bytes=%d, actual=%d", saved, sizeBefore-sizeAfter)
	}
	for _, h := range []domain.ContentHash{h1, h2} {
		got, gerr := st.GetDoc(ctx, h)
		if gerr != nil {
			t.Fatalf("repack GetDoc(%s): %v", h, gerr)
		}
		if err := domain.ValidateSessionDocHash(got); err != nil {
			t.Fatalf("hash mismatch: %v", err)
		}
	}
	// idempotence: running again results in 0 conversions.
	if n2, _, _ := st.RepackDocs(); n2 != 0 {
		t.Fatalf("repack idempotence violation: second pass converted %d documents", n2)
	}
}

func TestRepackDocsUpgradesV1ManifestToV2(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)
	canonical, err := domain.CanonicalBytes(bigDoc(40))
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(canonical)
	plan, ok := chunkcas.PlanDocV1(canonical)
	if !ok {
		t.Fatal("v1 test plan unavailable")
	}
	for _, chunkHash := range plan.Order {
		if err := writeAtomic(st.objectPath("chunks", chunkHash), docCompress(plan.Bodies[chunkHash])); err != nil {
			t.Fatal(err)
		}
	}
	manifest, _ := json.Marshal(plan.Manifest)
	if err := writeAtomic(st.objectPath("docs", hash), docCompress(manifest)); err != nil {
		t.Fatal(err)
	}

	converted, _, err := st.RepackDocs()
	if err != nil || converted != 1 {
		t.Fatalf("v1 repack converted=%d err=%v", converted, err)
	}
	raw, err := readCxtFile(st.objectPath("docs", hash))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = docDecompress(raw)
	gotManifest, isManifest := chunkcas.ParseManifest(raw)
	if !isManifest || gotManifest.Format != chunkcas.FormatV2 {
		t.Fatalf("manifest=%+v isManifest=%v, want v2", gotManifest, isManifest)
	}
	got, err := st.GetDoc(ctx, hash)
	if err != nil || domain.ValidateSessionDocHash(got) != nil {
		t.Fatalf("post-upgrade roundtrip err=%v", err)
	}
}

func TestRepackDocsPreservesMonolithOnChunkCollision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)
	canonical, _ := domain.CanonicalBytes(bigDoc(40))
	hash := domain.HashContent(canonical)
	if err := writeAtomic(st.objectPath("docs", hash), docCompress(canonical)); err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(canonical)
	if !ok {
		t.Fatal("v2 plan unavailable")
	}
	if err := writeAtomic(st.objectPath("chunks", plan.Order[0]), docCompress([]byte("wrong body"))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RepackDocs(); err == nil {
		t.Fatal("chunk collision was not detected")
	}
	got, err := st.GetDoc(ctx, hash)
	if err != nil || domain.ValidateSessionDocHash(got) != nil {
		t.Fatalf("source monolith was not preserved: %v", err)
	}
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
