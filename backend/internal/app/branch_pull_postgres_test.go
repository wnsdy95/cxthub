//go:build postgres

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Run only with an explicitly supplied test database. Compilation is also
// useful without a database; ordinary candidate tests do not execute this case.
func TestPGBranchPullPlanReadsOneGeneration(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
	defer cancel()
	base := collaborationSnapshot(t, st, repo, "base")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}
	if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	memory := domain.MemoryDigest{SnapshotID: base, Summary: "before"}
	oldHash, err := st.PutMemory(ctx, repo, memory)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapSnapshotMemory(ctx, repo, base, "", oldHash); err != nil {
		t.Fatal(err)
	}
	oldSnapshot, err := st.GetSnapshot(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := domain.SnapshotStateHash(oldSnapshot)
	if err != nil || oldSnapshot.MemoryHash != oldHash {
		t.Fatal("old attachment fixture not frozen", err)
	}
	event := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "position", Target: base, GitAfter: strings.Repeat("1", 40), MemoryHash: oldHash, MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
	if err := st.ApplyHistoryEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	paused := &pausedViewPG{PostgresStore: st, read: make(chan struct{}), resume: make(chan struct{})}
	reader := NewService(paused, st, nil, nil, nil)
	if reader.BranchPullVersion() != 1 {
		t.Fatal("PG capability absent")
	}
	type result struct {
		plan domain.BranchPullPlan
		err  error
	}
	results := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p, e := reader.PullBranchPlan(ctx, repo, domain.BranchPullRequest{Version: 1, Branch: "main"})
		results <- result{p, e}
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-paused.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// All changes commit after the plan has read refs, before it reads history,
	// snapshots. Tokens must freeze the old attachment hash; no memory bodies
	// are read by the planner.
	next := collaborationSnapshot(t, st, repo, "after", base)
	memory.Summary = "after"
	memory.PreviousMemoryHash = oldHash
	newHash, err := st.PutMemory(ctx, repo, memory)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WithinRepository(ctx, repo, func(tx context.Context) error {
		event.ID = strings.Repeat("b", 32)
		event.MemoryHash = newHash
		event.CreatedAt = time.Unix(2, 0)
		if err := st.ApplyHistoryEvent(tx, event); err != nil {
			return err
		}
		if err := st.CompareAndSwapSnapshotMemory(tx, repo, base, oldHash, newHash); err != nil {
			return err
		}
		ref.Target = next
		return st.CompareAndSwapRef(tx, repo, ref, base)
	}); err != nil {
		t.Fatal(err)
	}
	close(paused.resume)
	var old result
	select {
	case old = <-results:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if old.err != nil {
		t.Fatal(old.err)
	}
	if old.plan.SelectedRef.Target != base || len(old.plan.History) != 1 || len(old.plan.SnapshotIndex) != 1 || old.plan.SnapshotStates[base] != oldToken || old.plan.History[0].MemoryHash != oldHash {
		t.Fatalf("mixed read generations: %+v", old.plan)
	}
	newSnapshot, err := st.GetSnapshot(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	newToken, err := domain.SnapshotStateHash(newSnapshot)
	if err != nil || newSnapshot.MemoryHash != newHash || newToken == oldToken {
		t.Fatal("new attachment fixture not frozen", err)
	}
	current, err := svc.PullBranchPlan(ctx, repo, domain.BranchPullRequest{Version: 1, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if current.SelectedRef.Target != next || len(current.History) != 2 || len(current.SnapshotIndex) != 2 || current.SnapshotStates[base] != newToken || current.History[0].MemoryHash != oldHash || current.History[1].MemoryHash != newHash {
		t.Fatalf("new committed generation absent: %+v", current)
	}
}
