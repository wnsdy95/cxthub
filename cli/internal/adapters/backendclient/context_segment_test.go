package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func segmentWireFixture(t *testing.T) ([]byte, domain.ContextQueryView) {
	t.Helper()
	raw, err := os.ReadFile("../../../../schemas/testdata/context-segment-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var view domain.ContextQueryView
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	return raw, view
}
func TestContextSegmentsServerWireRoundTrip(t *testing.T) {
	raw, want := segmentWireFixture(t)
	repo := want.Snapshots[0].RepoID
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("segment_limit") != "3" || q.Get("segment_offset") != "0" || q.Get("segment_state_hash") != "" || q.Get("branch") != "main" || q.Get("position") != string(want.Position) || q.Get("code_commit") != want.Inclusion.CodeCommit {
			t.Errorf("unexpected segment request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "token" }, domain.TeamIdentity{})
	in := domain.ContextSelection{SegmentLimit: 3, Position: string(want.Position), Branch: "main", Scope: "current", CodeCommit: want.Inclusion.CodeCommit}
	got, err := c.QueryContext(context.Background(), repo, in)
	if err != nil || !reflect.DeepEqual(got, want) || calls != 1 {
		t.Fatalf("backend-generated wire round trip: %v calls=%d", err, calls)
	}
	if got.Revision.Evidence != 9007199254740993 || got.Segments.Entries[0].Kind != "verified_prefix" || got.Segments.Entries[2].TotalEvents != -1 {
		t.Fatal("wire facts changed")
	}
	wrapped := domain.HistoryQueryResult{Version: 1, Segments: got.Segments}
	body, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var again domain.HistoryQueryResult
	if err = json.Unmarshal(body, &again); err != nil || !reflect.DeepEqual(again.Segments, want.Segments) {
		t.Fatal("history wrapper loses segments")
	}
}
func TestContextSegmentsPageContinuationPinsStateAndOrder(t *testing.T) {
	_, fixture := segmentWireFixture(t)
	repo := fixture.Snapshots[0].RepoID
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		offset, err := strconv.Atoi(q.Get("segment_offset"))
		if err != nil || offset < 0 || offset > 2 {
			t.Errorf("bad offset %s", q.Get("segment_offset"))
			http.Error(w, "bad", 400)
			return
		}
		if offset > 0 && q.Get("segment_state_hash") != string(fixture.StateHash) {
			t.Fatal("unpinned continuation")
		}
		view := fixture
		page := *fixture.Segments
		page.Offset = offset
		page.Entries = page.Entries[offset : offset+1]
		page.Complete = offset == 2
		page.Next = offset + 1
		if page.Complete {
			page.Next = -1
		}
		page.PageHash, err = domain.ContextSegmentPageHash(page)
		if err != nil {
			t.Fatal(err)
		}
		view.Segments = &page
		view.Snapshots = fixture.Snapshots[offset : offset+1]
		view.Inclusion = nil
		_ = json.NewEncoder(w).Encode(view)
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "token" }, domain.TeamIdentity{})
	in := domain.ContextSelection{SegmentLimit: 1, Scope: "current", Position: string(fixture.Position)}
	for i := 0; i < 3; i++ {
		view, err := c.QueryContext(context.Background(), repo, in)
		if err != nil {
			t.Fatal(err)
		}
		page := view.Segments
		if page.Entries[0].SnapshotID != fixture.Snapshots[i].ID {
			t.Fatal("page reordered")
		}
		if i < 2 {
			in.SegmentOffset, in.SegmentStateHash = page.Next, page.StateHash
		} else if !page.Complete || page.Next != -1 {
			t.Fatal("missing terminal state")
		}
	}
	if calls != 3 {
		t.Fatal("unexpected retry or fallback")
	}
}
func TestContextSegmentsRejectMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ContextQueryView)
	}{
		{"old server", func(v *domain.ContextQueryView) { v.Segments = nil }},
		{"unknown version", func(v *domain.ContextQueryView) { v.Segments.Version = 2 }},
		{"wrong state", func(v *domain.ContextQueryView) { v.Segments.StateHash = domain.HashContent([]byte("other")) }},
		{"range beyond total", func(v *domain.ContextQueryView) { v.Segments.Entries[0].Segment.EventEnd++ }},
		{"negative range", func(v *domain.ContextQueryView) { v.Segments.Entries[0].Segment.EventStart = -1 }},
		{"foreign repo", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0].Segment.RepoID = domain.HashContent([]byte("foreign"))
		}},
		{"foreign document", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0].Segment.DocHash = domain.HashContent([]byte("foreign"))
		}},
		{"unknown baseline", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0].BaselineSnapshotID = domain.HashContent([]byte("foreign"))
		}},
		{"wrong baseline document", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0].BaselineDocHash = domain.HashContent([]byte("foreign"))
		}},
		{"wrong binding", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0].Bindings[0].SegmentIDs = []domain.ContentHash{domain.HashContent([]byte("foreign"))}
		}},
		{"invalid publication", func(v *domain.ContextQueryView) { v.Segments.Entries[0].Bindings[0].GitCommit = "short" }},
		{"unavailable range", func(v *domain.ContextQueryView) { v.Segments.Entries[2].Segment = v.Segments.Entries[0].Segment }},
		{"full source cut", func(v *domain.ContextQueryView) { v.Segments.Entries[1].Segment.EventStart = 1 }},
		{"unknown coverage", func(v *domain.ContextQueryView) { v.Segments.Entries[0].Kind = "guessed" }},
		{"order mismatch", func(v *domain.ContextQueryView) {
			v.Segments.Entries[0], v.Segments.Entries[1] = v.Segments.Entries[1], v.Segments.Entries[0]
		}},
		{"offset mismatch", func(v *domain.ContextQueryView) { v.Segments.Offset = 1 }},
		{"total mismatch", func(v *domain.ContextQueryView) { v.Segments.Total++ }},
		{"nonterminal exhaustion", func(v *domain.ContextQueryView) { v.Segments.Complete = false }},
		{"nonprogress cursor", func(v *domain.ContextQueryView) { v.Segments.Next = 0 }},
		{"missing entry", func(v *domain.ContextQueryView) { v.Segments.Entries = v.Segments.Entries[:2] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, view := segmentWireFixture(t)
			repo := view.Snapshots[0].RepoID
			tc.mutate(&view)
			if view.Segments != nil {
				view.Segments.PageHash, _ = domain.ContextSegmentPageHash(*view.Segments)
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(view) }))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			if _, err := c.QueryContext(context.Background(), repo, domain.ContextSelection{SegmentLimit: 3}); err == nil {
				t.Fatal("malformed segment contract accepted")
			}
		})
	}
}
func TestContextSegmentsRejectInvalidRequestBeforeIO(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected", 500) }))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte("repo")))
	for _, in := range []domain.ContextSelection{{SegmentLimit: 51}, {SegmentLimit: -1}, {SegmentLimit: 1, SegmentOffset: -1}, {SegmentOffset: 1}, {SegmentLimit: 1, SegmentOffset: 1}, {SegmentLimit: 1, SegmentStateHash: "broken"}} {
		if _, err := c.QueryContext(context.Background(), repo, in); err == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid selection sent %d requests", calls)
	}
}

func TestContextSegmentsRejectStaleGenerationAndChangedHashes(t *testing.T) {
	for _, mode := range []string{"stale generation", "page hash", "segment hash"} {
		t.Run(mode, func(t *testing.T) {
			_, view := segmentWireFixture(t)
			repo := view.Snapshots[0].RepoID
			in := domain.ContextSelection{SegmentLimit: 3}
			switch mode {
			case "stale generation":
				in.SegmentStateHash = domain.HashContent([]byte("older generation"))
			case "page hash":
				view.Segments.PageHash = domain.HashContent([]byte("different metadata"))
			case "segment hash":
				view.Segments.Entries[0].Segment.ID = domain.HashContent([]byte("different source range"))
				view.Segments.Entries[0].Bindings[0].SegmentIDs = []domain.ContentHash{view.Segments.Entries[0].Segment.ID}
				view.Segments.PageHash, _ = domain.ContextSegmentPageHash(*view.Segments)
			}
			if err := domain.ValidateContextQuery(repo, in, view); err == nil {
				t.Fatal("stale or changed content identity accepted")
			}
		})
	}
}
