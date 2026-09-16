package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
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
	if err := saveRewrites(cwd, map[string]string{old: newOID}); err != nil {
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
	count := 0
	for _, e := range events {
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
