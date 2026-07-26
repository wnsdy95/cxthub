package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// TestSearch locks in the core search contract: commit-message matches (kind=commit),
// event body match (kind=event, case-insensitive), deduplication of inheritance events (doc sharing·prefix duplication) to the first snapshot,
// rejection of messages shorter than 2 characters.
func TestSearch(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	svc := NewService(fs, fs, nil, nil, nil)
	repoID := h('0')

	msg := func(role domain.Role, text string, seq int) domain.CIREvent {
		return domain.CIREvent{Kind: domain.EventMessage, Seq: seq, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
	}
	// doc1: parent session. doc2: inheritance (events from doc1 as prefix) + new event.
	doc1 := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{
		msg(domain.RoleUser, "How to pin Webhook SSRF?", 0),
		msg(domain.RoleAssistant, "IP validation in dialer", 1),
	}}}
	doc2 := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{
		doc1.CIR.Events[0], doc1.CIR.Events[1],
		msg(domain.RoleUser, "What about Webhook loss?", 2),
	}}}
	for _, doc := range []*domain.SessionDoc{&doc1, &doc2} {
		canonical, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(canonical)
	}
	for _, d := range []domain.SessionDoc{doc1, doc2} {
		if _, err := fs.PutDoc(ctx, repoID, d); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().UTC().Add(-time.Hour)
	snaps := []domain.Snapshot{
		{ID: doc1.Hash, RepoID: repoID, Branch: "main", DocHash: doc1.Hash, Message: "feat: Webhook SSRF defense", CreatedAt: t0},
		{ID: doc2.Hash, RepoID: repoID, Branch: "main", DocHash: doc2.Hash, Parents: []domain.ContentHash{doc1.Hash}, Message: "fix: follow-up", CreatedAt: t0.Add(time.Minute)},
	}
	for _, sn := range snaps {
		if err := fs.PutSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
	}

	// Reject queries shorter than two characters.
	if _, err := svc.Search(ctx, inbound.SearchInput{RepoID: repoID, Query: "x"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("1-character search not rejected: %v", err)
	}

	// "ssrf" → 1 commit (snapshot a) + 1 event (user question, doc share dedup to 1 hit).
	out, err := svc.Search(ctx, inbound.SearchInput{RepoID: repoID, Query: "ssrf"})
	if err != nil {
		t.Fatal(err)
	}
	var commits, events int
	for _, hit := range out.Hits {
		switch hit.Kind {
		case "commit":
			commits++
			if hit.SnapshotID != doc1.Hash {
				t.Fatalf("commit match assignment error: %s", hit.SnapshotID)
			}
		case "event":
			events++
			if hit.SnapshotID != doc1.Hash || hit.Role != "user" {
				t.Fatalf("event match not assigned to initial snapshot/author: %+v", hit)
			}
		}
	}
	if commits != 1 || events != 1 {
		t.Fatalf("ssrf hit count error: commits=%d events=%d hits=%+v", commits, events, out.Hits)
	}

	// "webhook" (case-insensitive) → new event from doc2 assigned to snapshot b.
	out, err = svc.Search(ctx, inbound.SearchInput{RepoID: repoID, Query: "webhook"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range out.Hits {
		if hit.Kind == "event" && hit.SnapshotID == doc2.Hash {
			found = true
		}
	}
	if !found {
		t.Fatalf("new event not assigned to child snapshot: %+v", out.Hits)
	}
}
