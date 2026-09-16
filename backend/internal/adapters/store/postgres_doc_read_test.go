//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func checkReadIndexPG(t *testing.T, s *PostgresStore, repo domain.ContentHash) {
	ctx := context.Background()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "Literal 50%_\\ \uac80\uc0c9 NeedLE"}}},
		{Kind: domain.EventToolResult, Seq: 2},
	}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := s.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	idx, err := s.DocReadIndex(ctx, repo, doc.Hash)
	if err != nil || len(idx.Events) != 2 {
		t.Fatalf("index: %v %+v", err, idx)
	}
	for _, q := range []string{"50%_\\", "\uac80\uc0c9", "needle"} {
		hits, err := s.SearchDocEvents(ctx, repo, doc.Hash, q, -1, 10)
		if err != nil || len(hits) != 1 || hits[0].Seq != 1 {
			t.Fatalf("literal %q: %v %+v", q, err, hits)
		}
		candidates, err := s.MatchingDocHashes(ctx, repo, q)
		if err != nil || !candidates[doc.Hash] {
			t.Fatalf("candidates %q: %v %+v", q, err, candidates)
		}
	}
	hits, err := s.SearchDocEvents(ctx, repo, doc.Hash, "needle", 0, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("ordinal continuation: %v %+v", err, hits)
	}
	foreign := domain.HashContent([]byte("foreign-index-reader"))
	if _, err = s.SearchDocEvents(ctx, foreign, doc.Hash, "needle", -1, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign search: %v", err)
	}
	candidates, err := s.MatchingDocHashes(ctx, foreign, "needle")
	if err != nil || len(candidates) != 0 {
		t.Fatal("candidate ownership leak")
	}
	child := doc
	child.CIR.Envelope.GitBranch = "index-child"
	raw, _ = domain.CanonicalBytes(child.CIR)
	child.Hash = domain.HashContent(raw)
	if _, err = s.PutDoc(ctx, repo, child); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events WHERE hash=$1`, idx.Events[0].Hash).Scan(&n); err != nil || n != 1 {
		t.Fatalf("shared event dedup: %d %v", n, err)
	}
	// Simulate a pre-migration document, then prove lazy rebuilding is lossless.
	if _, err = s.pool.Exec(ctx, `DELETE FROM doc_read_indexes WHERE hash=$1`, child.Hash); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := s.DocReadIndex(ctx, repo, child.Hash)
	if err != nil || len(rebuilt.Events) != 2 || rebuilt.Events[0].Hash != idx.Events[0].Hash {
		t.Fatalf("backfill: %v %+v", err, rebuilt)
	}
	if err = s.DeleteDoc(ctx, repo, child.Hash); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDoc(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events WHERE hash=$1`, idx.Events[0].Hash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan search text: %d %v", n, err)
	}
}
