package store

import (
	"bytes"
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamingSearchFilterPreservesLegacyBits(t *testing.T) {
	for _, text := range []string{"", "a", "ab", "abcdef", "\ud55c\uad6d\uc5b4 caf\u00e9 \U0001f44b", strings.Repeat("large seed ", 10000)} {
		want, got := make([]byte, 32768), make([]byte, 32768)
		for i := 0; i+3 <= len(text); i++ {
			h := fnv.New32a()
			_, _ = h.Write([]byte(text[i : i+3]))
			slot := h.Sum32() % (32768 * 8)
			want[slot/8] |= 1 << (slot % 8)
		}
		visitSearchTrigrams(text, func(slot uint32) bool { got[slot/8] |= 1 << (slot % 8); return true })
		if !bytes.Equal(got, want) {
			t.Fatal("filter incompatible with stored documents")
		}
	}
	if visitSearchTrigrams("missing", func(uint32) bool { return false }) {
		t.Fatal("missing trigram did not stop query")
	}
}

func BenchmarkStreamingSearchFilter(b *testing.B) {
	text := strings.Repeat("synthetic seed ", 1<<20)
	bits := make([]byte, 32768)
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for b.Loop() {
		visitSearchTrigrams(text, func(slot uint32) bool { bits[slot/8] |= 1 << (slot % 8); return true })
	}
}

func TestLegacyDocReadProjectionBackfillIsLosslessAndOwned(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("legacy-read-index"))
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "50%_\\ \uac80\uc0c9 NeedLE"}}}}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if err := writeAtomic(s.docPath(repo, doc.Hash), raw); err != nil {
		t.Fatal(err)
	}
	// A pre-fix index may contain text/roles from a different event ordering.
	// The new namespace must rebuild from the archive, not adopt that projection.
	legacyIndex := filepath.Join(s.repoDir(repo), "read-index-v1", hexOf(doc.Hash))
	for _, suffix := range []string{"", ".search", ".filter"} {
		if err := writeAtomic(legacyIndex+suffix, []byte("legacy index")); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	if err := s.BackfillReadIndexes(ctx, func(n int) { count = n }); err != nil || count != 1 {
		t.Fatalf("backfill: %d %v", count, err)
	}
	idx, err := s.DocReadIndex(ctx, repo, doc.Hash)
	if err != nil || len(idx.Events) != 1 || idx.Events[0].Text != "" {
		t.Fatalf("metadata projection: %v %+v", err, idx)
	}
	for _, q := range []string{"needle", "\uac80\uc0c9", "50%_\\", "e", "50"} {
		hits, err := s.SearchDocEvents(ctx, repo, doc.Hash, q, -1, 10)
		if err != nil || len(hits) != 1 {
			t.Fatalf("literal %q: %v %+v", q, err, hits)
		}
	}
	got, err := s.GetDoc(ctx, repo, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := domain.CanonicalBytes(got.CIR)
	if string(again) != string(raw) {
		t.Fatal("backfill changed archive")
	}
	man, err := s.GetDocManifest(ctx, repo, doc.Hash)
	if err != nil || man.Format != domain.ChunkFormatV2 {
		t.Fatal("legacy archive not available for range reads")
	}
	foreign := domain.HashContent([]byte("not-owner"))
	if _, err = s.DocReadIndex(ctx, foreign, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign index: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.BackfillReadIndexes(cancelled, func(int) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backfill: %v", err)
	}
	if err = s.DeleteDoc(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".search", ".filter"} {
		if _, err = os.Stat(s.readIndexPath(repo, doc.Hash) + suffix); !os.IsNotExist(err) {
			t.Fatalf("retained derived text: %s %v", suffix, err)
		}
		if _, err = os.Stat(legacyIndex + suffix); !os.IsNotExist(err) {
			t.Fatalf("retained legacy derived text: %v", err)
		}
	}
}
