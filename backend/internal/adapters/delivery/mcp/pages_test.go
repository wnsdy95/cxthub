package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pageBackend struct {
	fakeContextBackend
	docs   map[domain.ContentHash]domain.SessionDoc
	refs   []domain.Ref
	events []domain.HistoryEvent
}

func (f pageBackend) GetDoc(_ context.Context, repo, id domain.ContentHash) (domain.SessionDoc, error) {
	d, ok := f.docs[id]
	if !ok {
		return d, domain.ErrNotFound
	}
	return d, nil
}
func (f pageBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return f.refs, nil
}
func (f pageBackend) ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	return f.events, nil
}
func pageHash(i int) domain.ContentHash { return domain.ContentHash(fmt.Sprintf("sha256:%064x", i)) }

func TestRenamedBranchPagesPreserveIdentityAndExcludeReusedName(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	a, b, c := pageHash(2), pageHash(3), pageHash(4)
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo.ID), BranchID: "original", Branch: "old", Kind: "birth", Source: a, Target: a}
	rename := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: string(repo.ID), BranchID: birth.BranchID, Branch: "new", PreviousBranch: "old", BindingParent: birth.ID, Kind: "rename", Source: b, Target: b}
	reuse := birth
	reuse.ID, reuse.BranchID, reuse.BindingParent, reuse.Source, reuse.Target = strings.Repeat("c", 32), "reused", rename.ID, c, c
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {
		{ID: a, RepoID: repo.ID, Branch: "old"}, {ID: b, RepoID: repo.ID, Branch: "old", Parents: []domain.ContentHash{a}}, {ID: c, RepoID: repo.ID, Branch: "old"},
	}}}, refs: []domain.Ref{{Kind: domain.RefBranch, Name: "new", Target: b}, {Kind: domain.RefBranch, Name: "old", Target: c}}, events: []domain.HistoryEvent{reuse, rename, birth}}
	s := &Server{context: f}
	for _, name := range []string{"new", "old"} {
		got, err := s.scopeSnapshots(context.Background(), repo, toolArgs{Branch: name}, &pageCursor{})
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if name == "old" {
			want = 1
		}
		if len(got) != want {
			t.Fatalf("%s snapshots: %+v", name, got)
		}
		raw, err := s.historyPage(context.Background(), repo, toolArgs{Branch: name})
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Events []domain.HistoryEvent `json:"events"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != want {
			t.Fatalf("%s events: %+v", name, page.Events)
		}
		for _, e := range page.Events {
			if (e.BranchID == birth.BranchID) != (name == "new") {
				t.Fatal("logical tasks mixed")
			}
		}
	}
}

func TestMemoryPagesPinTheFirstVersionAndReassembleUnicode(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	id := pageHash(2)
	memory := domain.MemoryDigest{SnapshotID: id, Summary: strings.Repeat("\uAE30\uC5B5\uC744 \uBCF4\uC874\uD569\uB2C8\uB2E4. ", 3000)}
	hash, _ := domain.MemoryDigestHash(memory)
	snaps := []domain.Snapshot{{ID: id, RepoID: repo.ID, MemoryHash: hash}}
	memories := map[domain.ContentHash]domain.MemoryDigest{hash: memory}
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: snaps}, memories: memories}}
	s := &Server{context: f}
	a := toolArgs{Repository: string(repo.ID), Ref: string(id), Mode: "stored"}
	var combined strings.Builder
	for calls := 0; calls < 100; calls++ {
		raw, err := s.memoryPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Hash     domain.ContentHash `json:"memory_hash"`
			Offset   int                `json:"byte_offset"`
			Fragment string             `json:"json_fragment"`
			Next     string             `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if page.Hash != hash || page.Offset != combined.Len() || len(page.Fragment) > pageBytes {
			t.Fatalf("inconsistent memory page: %+v", page)
		}
		combined.WriteString(page.Fragment)
		// Another writer changes the mutable attachment between every read.
		newer := domain.MemoryDigest{SnapshotID: id, Summary: "later memory"}
		nextHash, _ := domain.MemoryDigestHash(newer)
		memories[nextHash] = newer
		snaps[0].MemoryHash = nextHash
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	want, _ := json.Marshal(memory)
	if combined.String() != string(want) {
		t.Fatal("paged memory changed version or lost bytes")
	}
}

func TestSearchContinuesPastEmptyPagesAndMidDocument(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	var snaps []domain.Snapshot
	docs := map[domain.ContentHash]domain.SessionDoc{}
	for i := 0; i < 102; i++ {
		id := pageHash(i + 2)
		snap := domain.Snapshot{ID: id, RepoID: repo.ID, DocHash: id, CreatedAt: time.Unix(int64(200-i), 0)}
		snaps = append(snaps, snap)
		doc := domain.SessionDoc{}
		if i == 101 {
			for j := 0; j < 5004; j++ {
				text := "unrelated"
				if j >= 5000 {
					text = "needle"
				}
				doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: j, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
			}
		}
		docs[id] = doc
	}
	s := &Server{context: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: snaps}}, docs: docs}}
	a := toolArgs{Repository: string(repo.ID), Query: "needle", Limit: 2}
	seen := map[int]bool{}
	emptyPages := 0
	for calls := 0; calls < 20; calls++ {
		raw, err := s.searchPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Hits []struct {
				Seq int `json:"seq"`
			} `json:"hits"`
			Next string `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) == 0 {
			emptyPages++
		}
		for _, hit := range page.Hits {
			if seen[hit.Seq] {
				t.Fatal("duplicate search hit")
			}
			seen[hit.Seq] = true
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	if len(seen) != 4 || emptyPages < 2 {
		t.Fatalf("search stopped early: hits=%v empty=%d", seen, emptyPages)
	}
}

func TestBranchScopeIncludesSharedContentAndRetainedAncestors(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	a, b, c := pageHash(2), pageHash(3), pageHash(4)
	snaps := []domain.Snapshot{{ID: a, RepoID: repo.ID, Branch: "main"}, {ID: b, RepoID: repo.ID, Branch: "another", Parents: []domain.ContentHash{a}}, {ID: c, RepoID: repo.ID, Branch: "another", Parents: []domain.ContentHash{a}}}
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: snaps}}, refs: []domain.Ref{{Kind: domain.RefBranch, Name: "shared", Target: b}}, events: []domain.HistoryEvent{{Branch: "shared", Source: c, Target: b}}}
	s := &Server{context: f}
	cur := pageCursor{}
	got, err := s.scopeSnapshots(context.Background(), repo, toolArgs{Branch: "shared"}, &cur)
	if err != nil || len(got) != 3 {
		t.Fatalf("deduplicated branch content lost: %+v %v", got, err)
	}
}

func TestContextPaginationVisitsBeyond100WithStableTieOrder(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	snaps := []domain.Snapshot{}
	for i := 0; i < 137; i++ {
		snaps = append(snaps, domain.Snapshot{ID: pageHash(i + 2), RepoID: repo.ID, CreatedAt: time.Unix(100, 0), Message: "hook: retained", Branch: "main"})
	}
	s := &Server{context: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: snaps}}}}
	a := toolArgs{Repository: string(repo.ID), Limit: 17}
	seen := map[domain.ContentHash]bool{}
	for calls := 0; calls < 20; calls++ {
		raw, err := s.contextPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Snapshots []struct {
				ID domain.ContentHash `json:"id"`
			} `json:"snapshots"`
			Next string `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Snapshots {
			if seen[row.ID] {
				t.Fatal("duplicate snapshot")
			}
			seen[row.ID] = true
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
		wrong := a
		wrong.Scope = "previous"
		if _, err := s.contextPage(context.Background(), repo, wrong); err == nil {
			t.Fatal("changed cursor filter accepted")
		}
		if _, err := s.contextPage(context.Background(), domain.Repo{ID: pageHash(999)}, a); err == nil {
			t.Fatal("cross-repository cursor accepted")
		}
	}
	if len(seen) != 137 {
		t.Fatalf("visited %d, want 137 including hook captures", len(seen))
	}
}

func TestEventPagesReassembleEveryOldAndOversizedEvent(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	id := pageHash(2)
	var doc domain.SessionDoc
	rawEvents := []json.RawMessage{}
	for i := 0; i < 125; i++ {
		text := fmt.Sprintf("event %d", i)
		if i == 31 {
			text = strings.Repeat("\uD55C\uAE00🌿", 9000)
		}
		raw, _ := json.Marshal(map[string]any{"seq": i, "kind": "message", "role": "user", "blocks": []map[string]string{{"type": "text", "text": text}}})
		rawEvents = append(rawEvents, raw)
	}
	raw, _ := json.Marshal(map[string]any{"cir": map[string]any{"events": rawEvents}})
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: "main"}
	s := &Server{context: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snap}}}, docs: map[domain.ContentHash]domain.SessionDoc{id: doc}}}
	a := toolArgs{Ref: string(id), Events: 11}
	reassembled := make([]string, len(doc.CIR.Events))
	completed := 0
	for calls := 0; calls < 100; calls++ {
		raw, err := s.eventPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Fragments []eventFragment `json:"fragments"`
			Next      string          `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		bytes := 0
		for _, fragment := range page.Fragments {
			if fragment.Offset != len(reassembled[fragment.Index]) {
				t.Fatal("fragment gap or duplicate")
			}
			reassembled[fragment.Index] += fragment.JSON
			bytes += len(fragment.JSON)
			if fragment.Complete {
				completed++
			}
		}
		if bytes > pageBytes {
			t.Fatalf("page budget exceeded: %d", bytes)
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	if completed != 125 {
		t.Fatalf("completed %d events", completed)
	}
	for i, event := range doc.CIR.Events {
		raw, _ := json.Marshal(event)
		if reassembled[i] != string(raw) {
			t.Fatalf("event %d did not roundtrip", i)
		}
	}
}

func TestPreviousScopeRequiresPositionAndPreservesRealReachability(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	a, b, d := pageHash(2), pageHash(3), pageHash(4)
	snaps := []domain.Snapshot{{ID: a, RepoID: repo.ID}, {ID: b, RepoID: repo.ID, Parents: []domain.ContentHash{a}}, {ID: d, RepoID: repo.ID, Parents: []domain.ContentHash{a}}}
	s := &Server{context: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: snaps}}, refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", Target: d}, {Kind: domain.RefTag, Name: "cxt/history/v1/retained/source", Target: b}}}}
	if _, err := s.contextPage(context.Background(), repo, toolArgs{Scope: "previous"}); err == nil {
		t.Fatal("inferred local HEAD")
	}
	raw, err := s.contextPage(context.Background(), repo, toolArgs{Scope: "previous", Position: string(d)})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Snapshots []struct {
			ID domain.ContentHash `json:"id"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Snapshots) != 1 || page.Snapshots[0].ID != b {
		t.Fatalf("previous scope: %s", raw)
	}
}
