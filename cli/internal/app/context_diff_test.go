package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestContextDiffFrozenIndexAndLatePending(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	ctx := context.Background()
	prior := f.entry(t, 1)
	frozen := f.entry(t, 2)
	f.index.Entries = []domain.StagedSession{frozen}
	f.index = f.index.WithRevision()
	final := f.index
	final.Entries = []domain.StagedSession{prior}
	final = final.WithRevision()
	f.commits = []domain.StagingCommit{{Version: domain.StagingVersion, ID: "final", Index: final, LocalFinalized: true, Position: f.position, CreatedAt: time.Unix(99, 0)}}
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: workingDoc(t, "session", 3).Hash, UpdatedAt: time.Unix(200, 0)}}
	staged, err := s.Diff(ctx, inbound.ContextDiffInput{Staged: true})
	if err != nil || len(staged.Changes) != 1 {
		t.Fatal(staged, err)
	}
	c := staged.Changes[0]
	if c.Baseline != "finalized_index" || c.State != "extended" || c.Added.Start != 1 || c.Added.End != 2 {
		t.Fatal(c)
	}
	f.onDoc = func() {
		f.onDoc = nil
		f.pending[0].Target = workingDoc(t, "session", 4).Hash
		f.pending[0].UpdatedAt = time.Unix(201, 0)
	}
	observed, err := s.Diff(ctx, inbound.ContextDiffInput{})
	if err != nil || len(observed.Changes) != 1 {
		t.Fatal(observed, err)
	}
	c = observed.Changes[0]
	if c.Baseline != "staged" || c.After != f.pending[0].Target || c.Added.Start != 2 || c.Added.End != 4 {
		t.Fatal("mixed or live provider range", c)
	}
	if staged.AfterRevision != f.index.Revision || observed.BeforeRevision != f.index.Revision || observed.Freshness.ServerChecked {
		t.Fatal(staged, observed)
	}
	raw, _ := json.Marshal(observed)
	if strings.Contains(string(raw), "PRIVATE_DO_NOT_PRINT") {
		t.Fatal("diff exposed source text")
	}
	status, err := s.Status(ctx, "")
	if err != nil || len(status.LocalCommits) != 1 || status.LocalCommits[0].ServerReceiptState != "unknown" {
		t.Fatal(status, err)
	}
}
func TestContextDiffRewoundPositionExcludesFutureFinalizedCoverage(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	entry := f.entry(t, 3)
	f.index.Entries = []domain.StagedSession{entry}
	f.index = f.index.WithRevision()
	future := f.position
	future.Snapshot = entry.DocHash
	f.commits = []domain.StagingCommit{{Version: domain.StagingVersion, ID: "future", Index: f.index, LocalFinalized: true, Position: future}}
	out, err := s.Diff(context.Background(), inbound.ContextDiffInput{Staged: true})
	if err != nil || len(out.Changes) != 1 {
		t.Fatal(out, err)
	}
	c := out.Changes[0]
	if c.Before != f.position.Snapshot || c.Added.Start != 1 || c.Added.End != 3 || c.State != "extended" {
		t.Fatal("future receipt removed selected contribution", c)
	}
}
func TestContextDiffCannotDisambiguateReusedSessionAcrossProviderFiles(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	one, two := f.entry(t, 2), f.entry(t, 2)
	two.SourceID = domain.HashContent([]byte("other-provider-file"))
	two.Key = domain.StagedSessionKey(two.Provider, two.SessionID, two.SourceID, two.Generation)
	f.index.Entries = []domain.StagedSession{one, two}
	f.index = f.index.WithRevision()
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: one.Provider, SessionID: one.SessionID, Target: workingDoc(t, "session", 3).Hash}}
	out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
	if err != nil || len(out.Changes) != 1 {
		t.Fatal(out, err)
	}
	if out.Changes[0].State != "unknown" || out.Changes[0].Reason != "ambiguous_capture_source" || out.Changes[0].CountsKnown {
		t.Fatal(out)
	}
}
func TestContextDiffMissingAndUnknownAreNotClean(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	ctx := context.Background()
	out, err := s.Diff(ctx, inbound.ContextDiffInput{})
	if err != nil || len(out.Changes) != 0 || !containsWorkingGap(out.Gaps, "no_stored_pending_observation") || out.Freshness.WatcherState != "unknown" {
		t.Fatal(out, err)
	}
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: domain.HashContent([]byte("unavailable"))}}
	out, err = s.Diff(ctx, inbound.ContextDiffInput{})
	if err != nil || len(out.Changes) != 1 || out.Changes[0].State != "unavailable" || out.Changes[0].CountsKnown {
		t.Fatal(out, err)
	}
	f.pending[0].Target = workingDoc(t, "session", 3).Hash
	delete(f.docs, f.position.Snapshot)
	out, err = s.Diff(ctx, inbound.ContextDiffInput{})
	if err != nil || out.Changes[0].Reason != "baseline_document_missing" || out.Changes[0].CountsKnown {
		t.Fatal(out, err)
	}
}
func TestContextDiffNewSessionDoesNotDeduplicateSameText(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	other := workingDoc(t, "different-session", 1)
	f.docs[other.Hash] = other
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "different-session", Target: other.Hash}}
	out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
	if err != nil || len(out.Changes) != 1 || out.Changes[0].State != "new_source" || out.Changes[0].Added.End != 1 {
		t.Fatal(out, err)
	}
	f.history.Complete = false
	out, err = s.Diff(context.Background(), inbound.ContextDiffInput{})
	if err != nil || out.Changes[0].State != "unknown" || out.Changes[0].Reason != "history_incomplete" {
		t.Fatal(out, err)
	}
}
func TestContextDiffCorruptImmutableBodyFailsClosed(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	doc := workingDoc(t, "session", 2)
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: doc.Hash}}
	doc.CIR.Events[0].Blocks[0].Text = "altered"
	f.docs[doc.Hash] = doc
	if _, err := s.Diff(context.Background(), inbound.ContextDiffInput{}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
}
func containsWorkingGap(gaps []string, want string) bool {
	for _, g := range gaps {
		if g == want {
			return true
		}
	}
	return false
}

func TestContextDiffDoesNotOpenOlderCumulativeVersionsToHideRewrite(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	newest := workingDoc(t, "session", 2)
	newest.CIR.Events[0].ID = "rewritten-event"
	raw, err := domain.CanonicalBytes(newest.CIR)
	if err != nil {
		t.Fatal(err)
	}
	newest.Hash = domain.HashContent(raw)
	f.docs[newest.Hash] = newest
	snapshot := domain.Snapshot{ID: newest.Hash, DocHash: newest.Hash, RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session"}
	f.history.Snapshots = append([]domain.Snapshot{snapshot}, f.history.Snapshots...)
	f.position.Snapshot = newest.Hash
	f.history.Position = newest.Hash
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: workingDoc(t, "session", 3).Hash}}
	reads := 0
	f.onDoc = func() { reads++ }
	out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
	if err != nil || out.Changes[0].State != "replacement" || out.Changes[0].Before != newest.Hash {
		t.Fatal(out, err)
	}
	if reads != 2 {
		t.Fatalf("opened %d documents, want current and latest selected only", reads)
	}
}
