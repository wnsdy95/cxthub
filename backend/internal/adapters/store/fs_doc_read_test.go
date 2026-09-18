package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"path/filepath"
	"testing"
)

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
