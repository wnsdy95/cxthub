package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type segmentReadSpy struct {
	*store.FSStore
	reads        int
	catalogReads int
}

func (s *segmentReadSpy) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	s.catalogReads++
	return s.FSStore.ListSnapshots(ctx, repo, branch)
}
func (s *segmentReadSpy) DocReadIndex(ctx context.Context, r, h domain.ContentHash) (domain.DocReadIndex, error) {
	s.reads++
	return s.FSStore.DocReadIndex(ctx, r, h)
}
func segmentCapture(t *testing.T, st *store.FSStore, repo domain.ContentHash, words []string, parents ...domain.ContentHash) domain.Snapshot {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SourceProvider: "codex", SessionOriginID: "synthetic-stream"}}}
	for i, w := range words {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: w}}})
	}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(systemTestContext(), repo, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Parents: parents, CreatedAt: time.Unix(int64(len(words)), 0).UTC()}
	if err := st.PutSnapshot(systemTestContext(), snap); err != nil {
		t.Fatal(err)
	}
	return snap
}
func segmentPublish(t *testing.T, svc *Service, repo, snapshot domain.ContentHash, n int) {
	t.Helper()
	e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", n), RepoID: string(repo), BranchID: "main-identity", Branch: "main", Kind: "position", Source: snapshot, Target: snapshot, GitAfter: strings.Repeat(fmt.Sprintf("%x", n), 40), CreatedAt: time.Unix(int64(n), 0).UTC()}
	if err := svc.RecordHistory(systemTestContext(), e); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordHistory(systemTestContext(), prPublication(e)); err != nil {
		t.Fatal(err)
	}
}
func TestContextSegmentPagesBindOnlyFinalizedVerifiedRanges(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	a := segmentCapture(t, st, repo, []string{"first"})
	b := segmentCapture(t, st, repo, []string{"first", "new contribution"}, a.ID)
	segmentPublish(t, svc, repo, a.ID, 1)
	segmentPublish(t, svc, repo, b.ID, 2)
	spy := &segmentReadSpy{FSStore: st}
	svc.blobs = spy
	svc.meta = spy
	plain, err := svc.QueryContext(ctx, repo, domain.ContextSelection{})
	if err != nil || plain.Segments != nil || spy.reads != 0 {
		t.Fatalf("ordinary query read docs: %d %v", spy.reads, err)
	}
	in := domain.ContextSelection{SegmentLimit: 1}
	first, err := svc.QueryContext(ctx, repo, in)
	if err != nil {
		t.Fatal(err)
	}
	page := first.Segments
	if page == nil || page.Total != 2 || page.Next != 1 || page.Complete || len(page.Entries) != 1 || page.Entries[0].Kind != "verified_prefix" {
		t.Fatalf("first page: %+v", page)
	}
	entry := page.Entries[0]
	if entry.Segment.EventStart != 1 || entry.Segment.EventEnd != 2 || entry.BaselineSnapshotID != a.ID || len(entry.Bindings) != 1 || entry.Bindings[0].GitCommit != strings.Repeat("2", 40) {
		t.Fatalf("bad contribution %+v", entry)
	}
	if spy.reads != 2 {
		t.Fatalf("index reads=%d, want one source and one baseline", spy.reads)
	}
	events, err := svc.ReadDocEvents(ctx, repo, entry.Segment.DocHash, "", entry.Segment.EventStart, entry.Segment.EventEnd-entry.Segment.EventStart)
	if err != nil || len(events.Events) != 1 || events.Events[0].Blocks[0].Text != "new contribution" {
		t.Fatalf("range fetch: %+v %v", events, err)
	}
	catalogReads := spy.catalogReads
	in.SegmentOffset, in.SegmentStateHash = page.Next, page.StateHash
	last, err := svc.QueryContext(ctx, repo, in)
	if err != nil || !last.Segments.Complete || last.Segments.StateHash != page.StateHash || last.Segments.Entries[0].Kind != "full_source" {
		t.Fatalf("last page: %+v %v", last.Segments, err)
	}
	if spy.catalogReads != catalogReads {
		t.Fatalf("continuation reread full catalog: %d -> %d", catalogReads, spy.catalogReads)
	}
	if domain.ValidateContentHash(first.DeliveryStateHash) != nil || first.DeliveryStateHash != plain.DeliveryStateHash || last.DeliveryStateHash != first.DeliveryStateHash {
		t.Fatal("delivery proof must cover the entire selection on every page")
	}
	if len(last.Snapshots) != 1 || len(last.History) != 0 || last.Inclusion != nil {
		t.Fatal("continuation returned whole repository metadata")
	}
	// A late receipt changes coverage metadata without moving any context ref.
	segmentPublish(t, svc, repo, b.ID, 3)
	if _, err := svc.QueryContext(ctx, repo, in); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale receipt continuation accepted: %v", err)
	}
	fresh, err := svc.QueryContext(ctx, repo, domain.ContextSelection{SegmentLimit: 1})
	if err != nil || fresh.StateHash == first.StateHash || fresh.Segments.PageHash == page.PageHash {
		t.Fatalf("receipt did not invalidate hashes: %v", err)
	}
}
func TestContextSegmentsLegacyCoverageDoesNotInventDeltaOrBinding(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	a := segmentCapture(t, st, repo, []string{"same"})
	segmentCapture(t, st, repo, []string{"same", "later"}, a.ID)
	got, err := svc.QueryContext(ctx, repo, domain.ContextSelection{SegmentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got.Segments.Entries {
		if e.Kind != "full_source" || e.Segment.EventStart != 0 || len(e.Bindings) != 0 {
			t.Fatalf("legacy guessed: %+v", e)
		}
	}
	missing := domain.Snapshot{ID: hh("missing"), DocHash: hh("missing"), RepoID: repo, CreatedAt: time.Unix(10, 0)}
	if err := st.PutSnapshot(ctx, missing); err != nil {
		t.Fatal(err)
	}
	got, err = svc.QueryContext(ctx, repo, domain.ContextSelection{SegmentLimit: 1})
	if err != nil || got.Segments.Entries[0].Kind != "unavailable" || got.Segments.Entries[0].TotalEvents != -1 {
		t.Fatalf("missing hidden: %+v %v", got.Segments, err)
	}
}

func TestContextPublicationBindingsRequireExactAcceptedObservation(t *testing.T) {
	repo := hh(t.Name())
	snap := hh("capture")
	o := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: "b", Branch: "feature", Kind: "position", Target: snap, Source: snap, GitAfter: strings.Repeat("b", 40), WorktreeID: strings.Repeat("c", 32), CreatedAt: time.Unix(1, 0).UTC()}
	p := prPublication(o)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.HistoryEvent)
	}{
		{"other worktree", func(e *domain.HistoryEvent) { e.WorktreeID = strings.Repeat("d", 32) }},
		{"other branch identity", func(e *domain.HistoryEvent) { e.BranchID = "different" }},
		{"other code revision", func(e *domain.HistoryEvent) { e.GitAfter = strings.Repeat("e", 40) }},
		{"PR receipt is not capture observation", func(e *domain.HistoryEvent) { e.Kind = "pr-merge" }},
		{"other repository", func(e *domain.HistoryEvent) { e.RepoID = string(hh("foreign")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := o
			tc.mutate(&wrong)
			if got := contextPublicationBindings(repo, []domain.HistoryEvent{wrong, p}); len(got) != 0 {
				t.Fatal("unproven finalized binding")
			}
		})
	}
	if got := contextPublicationBindings(repo, []domain.HistoryEvent{o, p}); len(got[snap]) != 1 {
		t.Fatal("accepted binding missing")
	}
}

func TestContextSegmentCursorIgnoresUnrelatedCaptureTraffic(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	a := segmentCapture(t, st, repo, []string{"a"})
	b := segmentCapture(t, st, repo, []string{"a", "b"}, a.ID)
	segmentPublish(t, svc, repo, a.ID, 1)
	segmentPublish(t, svc, repo, b.ID, 2)
	in := domain.ContextSelection{Scope: "current", Position: string(b.ID), SegmentLimit: 1}
	got, err := svc.QueryContext(ctx, repo, in)
	if err != nil {
		t.Fatal(err)
	}
	in.SegmentOffset, in.SegmentStateHash = got.Segments.Next, got.Segments.StateHash
	c := segmentCapture(t, st, repo, []string{"unrelated pending"})
	segmentPublish(t, svc, repo, c.ID, 3)
	next, err := svc.QueryContext(ctx, repo, in)
	if err != nil || next.Segments.StateHash != got.Segments.StateHash {
		t.Fatalf("unrelated capture invalidated pinned content: %v", err)
	}
}
