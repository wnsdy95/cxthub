package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Real producer, durable replay and real selectors; no provider or remote server.
func TestRewriteInitialMemorySelectionReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	old, next := strings.Repeat("a", 40), strings.Repeat("b", 40)
	repo := string(domain.HashContent([]byte("rewrite-initial-memory-audit")))
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "feature", old)
	target, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "synthetic-audit"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, DocHash: target, RepoID: repo, Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "feature", Snapshot: target, SharedTarget: target, GitCommit: old, MemoryPinned: true}); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target, Summary: "first root memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitWorkingMemory(ctx, outbound.WorkingMemoryCommit{RepoID: before.RepoID, Snapshot: target, Memory: memory, ExpectedPosition: &before}); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !domain.IsInitialMemorySelection(*before.Selection, *after.Selection) {
		t.Fatal("producer did not create dedicated causal pair")
	}
	originals, err := st.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(originals) != 2 {
		t.Fatalf("unexpected original history: %d", len(originals))
	}
	svc := app.NewContextHistoryService(st, st)
	snap, err := st.GetSnapshot(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", BranchID: after.BranchID, Target: target}
	check := func(t *testing.T, code string, events []domain.HistoryEvent) {
		receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: repo, Kind: "pr-merge", PRCompleted: true, Branch: "main", BranchID: "base-id", SourceBranchID: after.BranchID, Source: target, Target: target, CreatedAt: time.Unix(3, 0).UTC(), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: code, MergeSHA: strings.Repeat("c", 40)}}
		p, err := svc.ResolvePRSourcePositionFromHistory(ctx, receipt, events)
		if err != nil || p.MemoryHash != memory {
			t.Errorf("PR selection at rewritten code lost M1: %v", err)
		}
		operation := domain.HistoryEvent{ID: strings.Repeat("e", 32), RepoID: repo, BranchID: "local-id", Branch: "local-feature", Kind: "attach", GitAfter: code, WorktreeID: strings.Repeat("d", 32), CreatedAt: time.Unix(4, 0).UTC()}
		got, err := svc.PrepareTrackingAttachment(ctx, operation, inbound.RemoteBranchObservation{Ref: ref, History: events, Snapshots: []domain.Snapshot{snap}}, []string{code})
		if err != nil || got.Event.MemoryHash != memory {
			t.Errorf("tracking selection at rewritten code lost M1: %v", err)
		}
	}
	t.Run("original-code-control", func(t *testing.T) { check(t, old, originals) })
	container := &Container{History: svc, List: app.NewListSessionsService(st)}
	if err := recordRewriteHistory(ctx, container, root, map[string]string{old: next}); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, container, root); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var aliases []domain.HistoryEvent
	for _, e := range rows {
		if e.GitAfter == next {
			aliases = append(aliases, e)
		}
	}
	if len(aliases) != 1 || aliases[0].MemoryHash != memory {
		t.Errorf("rewrite revived superseded empty selection: aliases=%d", len(aliases))
	}
	check(t, next, rows)
	if err := replayRewriteHistory(ctx, container, root); err != nil {
		t.Fatal(err)
	}
	again, err := st.ListHistoryEvents(ctx, repo)
	if err != nil || !reflect.DeepEqual(rows, again) {
		t.Fatalf("retry changed immutable history: %v", err)
	}
	for _, original := range originals {
		found := false
		for _, e := range rows {
			if e.ID == original.ID {
				found = reflect.DeepEqual(e, original)
			}
		}
		if !found {
			t.Fatal("original evidence rewritten")
		}
	}
}

// The delegate supplies real digest/source verification. Only history inventory
// and position are injected to exercise malformed durable input before writes.
type rewriteSelectionInventory struct {
	*app.ContextHistoryService
	events    []domain.HistoryEvent
	position  domain.WorkingPosition
	writes    int
	failAfter int
}

func (h *rewriteSelectionInventory) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	return h.position, nil
}
func (h *rewriteSelectionInventory) ListHistory(context.Context, string) ([]domain.HistoryEvent, error) {
	return append([]domain.HistoryEvent{}, h.events...), nil
}
func (h *rewriteSelectionInventory) RecordHistory(ctx context.Context, e domain.HistoryEvent) error {
	if h.failAfter > 0 && h.writes >= h.failAfter {
		return fmt.Errorf("test interrupted replay")
	}
	if err := h.ContextHistoryService.RecordHistory(ctx, e); err != nil {
		return err
	}
	h.events = append(h.events, e)
	h.writes++
	return nil
}

type rewriteSelectionUnavailable struct{ inbound.ContextHistory }

func rewriteSelectionFixture(t *testing.T) (string, *Container, *storage.FileStore, *rewriteSelectionInventory) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	target, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: t.Name()}}})
	if err != nil {
		t.Fatal(err)
	}
	repo := string(domain.HashContent([]byte("rewrite-scope")))
	if err := st.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: target, DocHash: target, Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	memory, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target, Summary: "root"})
	if err != nil {
		t.Fatal(err)
	}
	before := domain.HistoryEvent{RepoID: repo, ID: strings.Repeat("1", 32), BranchID: domain.LegacyContextBranchID(repo, "feature"), Branch: "feature", Kind: "position", Source: target, Target: target, MemoryPinned: true, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("2", 32), CreatedAt: time.Unix(10, 0).UTC()}
	after := before
	after.ID = strings.Repeat("3", 32)
	after.MemoryHash = memory
	after.MemorySelectionParent = before.ID
	after.CreatedAt = time.Unix(1, 0).UTC()
	h := &rewriteSelectionInventory{ContextHistoryService: app.NewContextHistoryService(st, st), events: []domain.HistoryEvent{after, before}, position: domain.WorkingPosition{RepoID: repo, BranchID: before.BranchID, Branch: before.Branch, WorktreeID: before.WorktreeID}}
	c := &Container{History: h, List: app.NewListSessionsService(st)}
	return root, c, st, h
}

func TestRewriteMemorySelectionDefersBeforeAliasWrites(t *testing.T) {
	for _, mode := range []string{"invalid", "missing", "nonroot", "foreign", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			root, c, st, h := rewriteSelectionFixture(t)
			ctx := context.Background()
			switch mode {
			case "invalid":
				h.events[0].MemoryPinned = false
			case "missing":
				h.events = h.events[:1]
			case "nonroot":
				m, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: h.events[0].Target, PreviousMemoryHash: h.events[0].MemoryHash, Summary: "nonroot"})
				if err != nil {
					t.Fatal(err)
				}
				h.events[0].MemoryHash = m
			case "foreign":
				m, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("foreign")), Summary: "foreign"})
				if err != nil {
					t.Fatal(err)
				}
				h.events[0].MemoryHash = m
			case "unavailable":
				c.History = rewriteSelectionUnavailable{h}
			}
			original := append([]domain.HistoryEvent{}, h.events...)
			if err := recordRewriteHistory(ctx, c, root, map[string]string{strings.Repeat("a", 40): strings.Repeat("b", 40)}); err != nil {
				t.Fatal(err)
			}
			if err := replayRewriteHistory(ctx, c, root); err == nil {
				t.Fatal("invalid rewrite was acknowledged")
			}
			if h.writes != 0 || !reflect.DeepEqual(h.events, original) {
				t.Fatal("wrote aliases before all initial-memory proofs passed")
			}
		})
	}
}

func TestRewriteMemorySelectionKeepsIndependentEmptyConflict(t *testing.T) {
	for _, mode := range []string{"unlinked", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			root, c, _, h := rewriteSelectionFixture(t)
			ctx := context.Background()
			old, next := strings.Repeat("a", 40), strings.Repeat("b", 40)
			if mode == "unlinked" {
				h.events[0].MemorySelectionParent = ""
			} else {
				other := h.events[1]
				other.ID = strings.Repeat("9", 32)
				h.events = append(h.events, other)
			}
			if err := recordRewriteHistory(ctx, c, root, map[string]string{old: next}); err != nil {
				t.Fatal(err)
			}
			if err := replayRewriteHistory(ctx, c, root); err != nil {
				t.Fatal(err)
			}
			var empty, nonempty bool
			for _, e := range h.events {
				if e.GitAfter == next {
					empty = empty || e.MemoryHash == ""
					nonempty = nonempty || e.MemoryHash != ""
				}
			}
			if !empty || !nonempty {
				t.Fatal("unlinked/independent empty observation suppressed")
			}
			receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: h.position.RepoID, Kind: "pr-merge", PRCompleted: true, Branch: "main", BranchID: "base", SourceBranchID: h.position.BranchID, Source: h.events[0].Target, Target: h.events[0].Target, CreatedAt: time.Unix(30, 0).UTC(), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: next, MergeSHA: strings.Repeat("c", 40)}}
			if _, err := h.ResolvePRSourcePositionFromHistory(ctx, receipt, h.events); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("explicit-empty conflict lost: %v", err)
			}
		})
	}
}

func TestRewriteMemorySelectionChainAndInterruptedReplay(t *testing.T) {
	root, c, _, h := rewriteSelectionFixture(t)
	ctx := context.Background()
	a, b, d := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("d", 40)
	if err := recordRewriteHistory(ctx, c, root, map[string]string{a: b, b: d}); err != nil {
		t.Fatal(err)
	}
	h.failAfter = 1
	if err := replayRewriteHistory(ctx, c, root); err == nil || h.writes != 1 {
		t.Fatalf("interruption not exercised: writes=%d err=%v", h.writes, err)
	}
	h.failAfter = 0
	if err := replayRewriteHistory(ctx, c, root); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{b, d} {
		count := 0
		for _, e := range h.events {
			if e.GitAfter == code {
				count++
				if e.MemoryHash == "" || e.MemorySelectionParent != "" || e.ID != rewriteObservationID(e) {
					t.Fatal("alias identity or selected memory changed")
				}
			}
		}
		if count != 1 {
			t.Fatalf("code %s aliases=%d", code, count)
		}
	}
	original := append([]domain.HistoryEvent{}, h.events...)
	if err := replayRewriteHistory(ctx, c, root); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, h.events) {
		t.Fatal("completed replay not idempotent")
	}
}

func TestRewriteMemorySelectionKeepsFullNativeAndKnownHistory(t *testing.T) {
	_, c, _, h := rewriteSelectionFixture(t)
	ctx := context.Background()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	// The superseded predecessor itself occupies B; its selected child also does.
	// A different native source at A must not be copied over this full-history fence.
	h.events[0].GitAfter = b
	h.events[1].GitAfter = b
	nativeSource := h.events[0]
	nativeSource.ID = strings.Repeat("9", 32)
	nativeSource.MemorySelectionParent = ""
	nativeSource.GitAfter = a
	h.events = append(h.events, nativeSource)
	excluded, err := rewriteMemoryExclusions(ctx, c.History, h.events, []rewriteBatch{{BranchID: h.position.BranchID, WorktreeID: h.position.WorktreeID}})
	if err != nil {
		t.Fatal(err)
	}
	if !excluded[h.events[1].ID] {
		t.Fatal("predecessor was not projected out")
	}
	got, err := rewrittenHistory(h.events, excluded, map[string]string{a: b}, h.position.BranchID, h.position.WorktreeID, time.Unix(40, 0).UTC())
	if err != nil || len(got) != 0 {
		t.Fatalf("native B overwritten: %d %v", len(got), err)
	}
	// Missing optional verifier remains supported when there is no new relation.
	h.events = []domain.HistoryEvent{nativeSource}
	if _, err := rewriteMemoryExclusions(ctx, rewriteSelectionUnavailable{h}, h.events, []rewriteBatch{{BranchID: h.position.BranchID, WorktreeID: h.position.WorktreeID}}); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteMemorySelectionRetainsEarlierEmptyAlias(t *testing.T) {
	root, c, _, h := rewriteSelectionFixture(t)
	ctx := context.Background()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	// This immutable alias was recorded before the original root memory arrived.
	// The now-complete source pair cannot retroactively remove that observation.
	early := h.events[1]
	early.GitBefore, early.GitAfter = a, b
	early.ID = rewriteObservationID(early)
	early.CreatedAt = time.Unix(20, 0).UTC()
	if err := h.RecordHistory(ctx, early); err != nil {
		t.Fatal(err)
	}
	h.writes = 0
	if err := recordRewriteHistory(ctx, c, root, map[string]string{a: b}); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, root); err != nil {
		t.Fatal(err)
	}
	retained := false
	nonempty := false
	for _, e := range h.events {
		if e.ID == early.ID {
			retained = reflect.DeepEqual(e, early)
		}
		if e.GitAfter == b && e.MemoryHash != "" {
			nonempty = true
		}
	}
	if !retained || !nonempty {
		t.Fatal("earlier empty alias was repaired or complete source was lost")
	}
	receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: h.position.RepoID, Kind: "pr-merge", PRCompleted: true, Branch: "main", BranchID: "base", SourceBranchID: h.position.BranchID, Source: early.Target, Target: early.Target, CreatedAt: time.Unix(30, 0).UTC(), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: b, MergeSHA: strings.Repeat("c", 40)}}
	if _, err := h.ResolvePRSourcePositionFromHistory(ctx, receipt, h.events); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("earlier ambiguous alias did not fail closed: %v", err)
	}
	original := append([]domain.HistoryEvent{}, h.events...)
	if err := replayRewriteHistory(ctx, c, root); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, h.events) {
		t.Fatal("retry changed retained ambiguous history")
	}
}
