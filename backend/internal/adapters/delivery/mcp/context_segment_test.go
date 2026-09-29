package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type segmentBackend struct {
	fakeContextBackend
	view      domain.ContextQueryView
	requested []domain.ContextSelection
}

func (b *segmentBackend) QueryContext(_ context.Context, _ domain.ContentHash, in domain.ContextSelection) (domain.ContextQueryView, error) {
	b.requested = append(b.requested, in)
	return b.view, nil
}
func TestContextSegmentMCPUsesSharedProjectionAndBoundsMetadata(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	entry := domain.ContextSegmentCoverage{SnapshotID: pageHash(2), Kind: "full_source", Reason: "no_verified_selected_baseline", Bindings: []domain.CommitContextBinding{}}
	for i := 0; i < 200; i++ {
		entry.Bindings = append(entry.Bindings, domain.CommitContextBinding{EventID: strings.Repeat("a", 32), GitCommit: strings.Repeat("b", 40), BranchID: strings.Repeat("C", 80), SegmentIDs: []domain.ContentHash{pageHash(3)}})
	}
	page := domain.ContextSegmentPage{Version: 1, StateHash: pageHash(4), PageHash: pageHash(5), Total: 1, Next: -1, Complete: true, Entries: []domain.ContextSegmentCoverage{entry}}
	b := &segmentBackend{view: domain.ContextQueryView{Version: 1, Position: entry.SnapshotID, Branch: "logical", Segments: &page}}
	s := &Server{context: b}
	a := toolArgs{Repository: string(repo.ID), Mode: "segments", Position: "main", Scope: "current", CodeCommit: strings.Repeat("e", 40)}
	rawPage := ""
	calls := 0
	for ; calls < 100; calls++ {
		raw, err := s.contextSegmentPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Offset   int    `json:"byte_offset"`
			Text     string `json:"json_fragment"`
			Complete bool   `json:"page_complete"`
			Next     string `json:"next_cursor"`
		}
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if p.Offset != len(rawPage) || len(p.Text) > pageBytes {
			t.Fatal("fragment gap or byte limit")
		}
		rawPage += p.Text
		if p.Next == "" {
			if !p.Complete {
				t.Fatal("partial page lost")
			}
			break
		}
		a.Cursor = p.Next
	}
	if calls < 1 || calls >= 100 {
		t.Fatal("oversized binding metadata did not paginate")
	}
	var got domain.ContextSegmentPage
	if err := json.Unmarshal([]byte(rawPage), &got); err != nil || !reflect.DeepEqual(got, page) {
		t.Fatalf("application projection changed: %v", err)
	}
	for _, in := range b.requested {
		if in.SegmentLimit != 1 || in.Scope != "current" || in.CodeCommit != a.CodeCommit {
			t.Fatalf("scope delegated incorrectly: %+v", in)
		}
	}
	if b.requested[1].Position != string(entry.SnapshotID) || b.requested[1].SegmentStateHash != page.StateHash {
		t.Fatal("continuation not pinned")
	}
	page.PageHash = pageHash(6)
	if _, err := s.contextSegmentPage(context.Background(), repo, a); err == nil {
		t.Fatal("changed page accepted midway")
	}
}

func TestContextFetchCanonicalRangeNeverLeaksAdjacentEvents(t *testing.T) {
	ctx := systemTestContext()
	repo := domain.Repo{ID: pageHash(40)}
	st := store.NewFSStore(t.TempDir())
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{}}}
	for i, word := range []string{"outside before", strings.Repeat("inside Unicode \ud55c\uae00 ", 2000), "outside after"} {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: word}}})
	}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo.ID, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo.ID}
	backend := &indexedPageBackend{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snap}}}}, reader: app.NewService(st, st, nil, nil, nil)}
	s := &Server{context: backend}
	start, end := 1, 2
	a := toolArgs{Ref: string(snap.ID), EventStart: &start, EventEnd: &end, Events: 50}
	body := ""
	for calls := 0; calls < 100; calls++ {
		raw, err := s.eventPage(ctx, repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Fragments []eventFragment `json:"fragments"`
			Next      string          `json:"next_cursor"`
		}
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		for _, f := range p.Fragments {
			if f.Index != 1 || f.Offset != len(body) || len(f.JSON) > pageBytes {
				t.Fatal("range leak or gap")
			}
			body += f.JSON
		}
		if p.Next == "" {
			break
		}
		a.Cursor = p.Next
	}
	var got domain.CIREvent
	if err := json.Unmarshal([]byte(body), &got); err != nil || !reflect.DeepEqual(got, doc.CIR.Events[1]) {
		t.Fatalf("range content changed: %v", err)
	}
	if backend.whole != 0 {
		t.Fatal("range read whole archive")
	}
	changed := 0
	bad := a
	bad.EventStart = &changed
	if _, err := s.eventPage(ctx, repo, bad); err == nil {
		t.Fatal("cursor reused with different range")
	}
	a.Cursor = ""
	a.EventEnd = &start
	emptyRaw, err := s.eventPage(ctx, repo, a)
	if err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Fragments []eventFragment `json:"fragments"`
		Next      string          `json:"next_cursor"`
	}
	if err = json.Unmarshal([]byte(emptyRaw), &empty); err != nil || len(empty.Fragments) != 0 || empty.Next != "" {
		t.Fatal("empty range returned content")
	}
	end = 4
	a.EventEnd = &end
	if _, err = s.eventPage(ctx, repo, a); err == nil {
		t.Fatal("out-of-document range accepted")
	}
}
