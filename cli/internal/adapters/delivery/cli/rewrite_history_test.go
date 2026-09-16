package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestRewrittenHistoryPreservesExactAssociations(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	original := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: "sha256:" + strings.Repeat("1", 64), BranchID: "feature-id", Branch: "feature", WorktreeID: strings.Repeat("2", 32), Kind: "position", Target: "sha256:" + strings.Repeat("3", 64), GitAfter: a, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	original.MemoryHash = "sha256:" + strings.Repeat("4", 64)
	otherBranch := original
	otherBranch.ID, otherBranch.BranchID = strings.Repeat("5", 32), "other-id"
	otherWorktree := original
	otherWorktree.ID, otherWorktree.WorktreeID = strings.Repeat("6", 32), strings.Repeat("7", 32)
	events := []domain.HistoryEvent{original, otherBranch, otherWorktree}
	now := time.Unix(2, 0).UTC()
	got, err := rewrittenHistory(events, map[string]string{a: b, b: c}, original.BranchID, original.WorktreeID, now)
	if err != nil || len(got) != 2 {
		t.Fatalf("rewrites: %v %v", got, err)
	}
	for i, e := range got {
		if err := domain.ValidateHistoryEvent(e); err != nil {
			t.Fatal(err)
		}
		if e.Target != original.Target || e.MemoryHash != original.MemoryHash || !e.MemoryPinned || e.BranchID != original.BranchID || e.GitAfter != []string{b, c}[i] || e.CreatedAt != now {
			t.Fatalf("association changed: %+v", e)
		}
	}
	if !reflect.DeepEqual(events[0], original) {
		t.Fatal("original mutated")
	}
	replay, err := rewrittenHistory(append(events, got...), map[string]string{a: b, b: c}, original.BranchID, original.WorktreeID, time.Now())
	if err != nil || len(replay) != 0 {
		t.Fatalf("replay duplicated observations: %v %v", replay, err)
	}
	fresh := original
	fresh.ID, fresh.GitAfter, fresh.Target = strings.Repeat("8", 32), b, "sha256:"+strings.Repeat("9", 64)
	delayed, err := rewrittenHistory(append(events, fresh), map[string]string{a: b, b: c}, original.BranchID, original.WorktreeID, now)
	if err != nil || len(delayed) != 1 || delayed[0].Target != fresh.Target || delayed[0].GitAfter != c {
		t.Fatalf("delayed replay overshadowed a fresh capture: %v %v", delayed, err)
	}
	for _, bad := range []map[string]string{{a: b, b: a}, {a: "abcdef"}, {a: strings.Repeat("0", 40)}} {
		if _, err := rewrittenHistory(events, bad, original.BranchID, original.WorktreeID, now); err == nil {
			t.Fatalf("accepted bad rewrite: %v", bad)
		}
	}
	got, err = rewrittenHistory(events, map[string]string{a[:7]: b}, original.BranchID, original.WorktreeID, now)
	if err != nil || len(got) != 0 {
		t.Fatal("short Git prefix used as exact evidence")
	}
}

func TestRewriteReplayResumesPartiallyPublishedSquash(t *testing.T) {
	a, b, squashed := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	first := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: "sha256:" + strings.Repeat("1", 64), BranchID: "feature", Branch: "feature", WorktreeID: strings.Repeat("2", 32), Kind: "position", Target: "sha256:" + strings.Repeat("3", 64), GitAfter: a, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	last := first
	last.ID, last.GitAfter, last.Target = strings.Repeat("4", 32), b, "sha256:"+strings.Repeat("5", 64)
	events := []domain.HistoryEvent{first, last}
	mapping := map[string]string{a: squashed, b: squashed}
	all, err := rewrittenHistory(events, mapping, first.BranchID, first.WorktreeID, time.Unix(2, 0).UTC())
	if err != nil || len(all) != 2 {
		t.Fatalf("prepare: %v %v", all, err)
	}
	// The process died after storing the first alias. The final commit's
	// conversation still has to arrive at the squashed Git revision on retry.
	remaining, err := rewrittenHistory(append(events, all[0]), mapping, first.BranchID, first.WorktreeID, time.Unix(3, 0).UTC())
	if err != nil || len(remaining) != 1 || remaining[0].Target != last.Target {
		t.Fatalf("partial squash lost final context: %+v %v", remaining, err)
	}
}

func TestRewritePublicationDoesNotBecomeMemorylessPosition(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	proof := domain.HistoryEvent{ID: strings.Repeat("1", 32), BranchID: "feature", Branch: "feature", WorktreeID: "worktree", Kind: "position", Target: domain.HashContent([]byte("context")), GitAfter: a, MemoryHash: domain.HashContent([]byte("memory")), MemoryPinned: true}
	published := proof
	published.ID, published.Kind, published.MemoryHash = strings.Repeat("2", 32), "publish", ""
	for _, at := range []string{a, b} {
		published.GitAfter = at
		got, err := rewrittenHistory([]domain.HistoryEvent{proof, published}, map[string]string{a: b}, proof.BranchID, proof.WorktreeID, time.Now())
		if err != nil || len(got) != 1 || got[0].MemoryHash != proof.MemoryHash || !got[0].MemoryPinned {
			t.Fatalf("publication at %s altered ordinary rewrite: %+v %v", at, got, err)
		}
	}
}

func TestIntermediateAmendCannotFinalizeBeforeSquashBoundary(t *testing.T) {
	cwd, c, _, repo, target := publicationFixture(t)
	ctx := context.Background()
	position, err := c.History.CurrentPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, next := gitOut(cwd, "rev-parse", "HEAD"), strings.Repeat("b", 40)
	p := domain.HistoryEvent{RepoID: repo, BranchID: position.BranchID, Branch: position.Branch, WorktreeID: position.WorktreeID, Kind: "publish", Source: target, Target: target, GitAfter: old, MemoryPinned: true}
	if err := persistPublication(ctx, c, cwd, p); err != nil {
		t.Fatal(err)
	}
	if err := recordRewriteBatch(ctx, c, cwd, map[string]string{old: next}, false); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	rows, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.Kind == "publish" && e.GitAfter == next {
			t.Fatal("intermediate amend prematurely finalized squash")
		}
	}
	// A later complete boundary permits replay; the real multi-input case is
	// exercised by the CLI commit/squash E2E and finalization unit test.
	if err := recordRewriteHistory(ctx, c, cwd, map[string]string{old: next}); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	rows, err = c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range rows {
		if e.Kind == "publish" && e.GitAfter == next {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("final publications = %d", count)
	}
}

type synchronizedRewriteHistory struct {
	inbound.ContextHistory
	mu      sync.Mutex
	readers int
	ready   chan struct{}
}

func (h *synchronizedRewriteHistory) ListHistory(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	events, err := h.ContextHistory.ListHistory(ctx, repo)
	h.mu.Lock()
	h.readers++
	n := h.readers
	if n == 2 {
		close(h.ready)
	}
	h.mu.Unlock()
	if n <= 2 {
		<-h.ready
	}
	return events, err
}

func TestConcurrentRewriteReplay(t *testing.T) {
	cwd, c, _, repo, target := historyFixture(t)
	ctx := context.Background()
	old := gitOut(cwd, "rev-parse", "HEAD")
	store := storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", old)
	history := app.NewContextHistoryService(store, store)
	if err := history.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: old, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	c.History = history
	if err := recordRewriteHistory(ctx, c, cwd, map[string]string{old: strings.Repeat("f", 40)}); err != nil {
		t.Fatal(err)
	}
	c.History = &synchronizedRewriteHistory{ContextHistory: history, ready: make(chan struct{})}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- replayRewriteHistory(ctx, c, cwd) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent replay failed: %v", err)
		}
	}
	events, err := history.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.GitAfter == strings.Repeat("f", 40) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("aliases = %d", count)
	}
}

func TestRewriteReplayIncludesInactiveWorktree(t *testing.T) {
	cwd, c, _, repo, target := historyFixture(t)
	ctx := context.Background()
	old := gitOut(cwd, "rev-parse", "HEAD")
	first := storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", old)
	c.History = app.NewContextHistoryService(first, first)
	if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: old, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	p, _ := c.History.CurrentPosition(ctx)
	if err := recordRewriteHistory(ctx, c, cwd, map[string]string{old: strings.Repeat("f", 40)}); err != nil {
		t.Fatal(err)
	}
	// A different worktree pushes after the original one is no longer active.
	second := storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git", "worktrees", "other"), "main", old)
	c.History = app.NewContextHistoryService(second, second)
	if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: old, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.GitAfter == strings.Repeat("f", 40) && e.WorktreeID == p.WorktreeID {
			return
		}
	}
	t.Fatal("push from another worktree left a durable rewrite unpublished")
}

type unavailableRewriteHistory struct{ inbound.ContextHistory }

func (h unavailableRewriteHistory) RecordHistory(context.Context, domain.HistoryEvent) error {
	return errors.New("simulated interrupted history storage")
}

func TestRewriteReplayStorageFailureRetainsJournal(t *testing.T) {
	cwd, c, _, repo, target := historyFixture(t)
	ctx := context.Background()
	old := gitOut(cwd, "rev-parse", "HEAD")
	store := storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", old)
	history := app.NewContextHistoryService(store, store)
	c.History = history
	if err := history.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: old, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	if err := recordRewriteHistory(ctx, c, cwd, map[string]string{old: strings.Repeat("f", 40)}); err != nil {
		t.Fatal(err)
	}
	c.History = unavailableRewriteHistory{history}
	if err := replayRewriteHistory(ctx, c, cwd); err == nil {
		t.Fatal("acknowledged failed history write")
	}
	// Reconstruct the service/store as a restarted CLI would.
	store = storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", old)
	c.History = app.NewContextHistoryService(store, store)
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.GitAfter == strings.Repeat("f", 40) {
			return
		}
	}
	t.Fatal("restarted replay lost the exact source")
}

func TestRewriteReplayDuringRebaseWithoutProvider(t *testing.T) {
	cwd, c, store, repo, target := historyFixture(t)
	ctx := context.Background()
	old := gitOut(cwd, "rev-parse", "HEAD")
	store = storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", old)
	c.History = app.NewContextHistoryService(store, store)
	if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: old, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	before, _ := c.History.CurrentPosition(ctx)
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "rewritten")
	newOID := gitOut(cwd, "rev-parse", "HEAD")
	if err := recordRewriteHistory(ctx, c, cwd, map[string]string{old: newOID}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".git", "rebase-merge"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// A shared legacy map from another worktree must not create a binding.
	if err := saveRewrites(cwd, map[string]string{newOID: strings.Repeat("e", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := replayRewriteHistory(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	events, _ = store.ListHistoryEvents(ctx, repo)
	count := 0
	for _, e := range events {
		if e.GitAfter == strings.Repeat("e", 40) {
			t.Fatal("shared map escaped its worktree")
		}
		if e.GitAfter == newOID {
			count++
			if e.Target != target || e.GitBefore != old {
				t.Fatalf("wrong source: %+v", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("rewritten observations = %d, want 1", count)
	}
	after, _ := c.History.CurrentPosition(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("replay moved the worktree's position")
	}
}
