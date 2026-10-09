package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func frozenCommitLaterJob(f frozenFixture) (domain.WorkingPosition, domain.HistoryEvent) {
	next := f.op.Position
	next.GitCommit = strings.Repeat("d", 40)
	selection := *next.Selection
	selection.Kind = "position"
	selection.GitAfter = next.GitCommit
	next.Selection = &selection
	event := domain.HistoryEvent{ID: strings.Repeat("e", 32), Kind: "advance", RepoID: f.repo,
		Branch: next.Branch, BranchID: next.BranchID, WorktreeID: next.WorktreeID,
		Source: f.position.SharedTarget, Target: next.Snapshot, MemoryPinned: true,
		GitBefore: f.position.GitCommit, GitAfter: next.GitCommit, CreatedAt: time.Unix(100, 0).UTC()}
	return next, event
}

func TestFrozenCommitPreservesLaterJobCodeWithoutMutatingWorker(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	startupBranch, startupCode := f.store.gitBranch, f.store.gitCommit
	next, event := frozenCommitLaterJob(f)
	originalNext, originalEvent := next, event
	if next.GitCommit == startupCode {
		t.Fatal("fixture must outlive its startup code")
	}
	if err := f.store.CommitFrozenSnapshotIfCurrent(ctx, f.op.Ref, f.position.SharedTarget, before, next, &event); err != nil {
		t.Fatal("apply later job through existing worker", err)
	}
	got, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(got, next) {
		t.Fatalf("frozen position changed: got=%+v want=%+v err=%v", got, next, err)
	}
	ref, err := f.store.GetRef(ctx, f.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != next.Snapshot {
		t.Fatal("later job did not atomically advance the shared target", err)
	}
	events, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, got := range events {
		if got.ID == event.ID {
			found = true
			if !reflect.DeepEqual(got, event) {
				t.Fatalf("advance adopted stale process code: got=%+v want=%+v", got, event)
			}
		}
	}
	if !found || f.store.gitBranch != startupBranch || f.store.gitCommit != startupCode || !reflect.DeepEqual(next, originalNext) || !reflect.DeepEqual(event, originalEvent) {
		t.Fatal("frozen commit omitted its event or mutated the shared worker/caller values")
	}
}

func TestFrozenCommitLaterJobKeepsSelectionAndRefFences(t *testing.T) {
	for _, changed := range []string{"memory-repin", "selection-only", "shared-target", "original-worktree", "next-worktree", "branch", "branch-identity", "invalid-code"} {
		t.Run(changed, func(t *testing.T) {
			f := newFrozenFixture(t)
			ctx := context.Background()
			original, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			next, event := frozenCommitLaterJob(f)
			switch changed {
			case "memory-repin":
				current := original
				current.MemoryHash, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: current.Snapshot, Summary: "concurrent selected memory"})
				if err != nil {
					t.Fatal(err)
				}
				current.MemorySource, current.MemoryPinned, current.Selection = current.Snapshot, true, nil
				if err := f.store.PutWorkingPosition(ctx, current); err != nil {
					t.Fatal(err)
				}
			case "selection-only":
				current := original
				selection := *current.Selection
				selection.ID = strings.Repeat("f", 32)
				current.Selection = &selection
				if err := f.store.PutWorkingPosition(ctx, current); err != nil {
					t.Fatal(err)
				}
			case "shared-target":
				if err := f.store.PutRef(ctx, f.op.Ref); err != nil {
					t.Fatal(err)
				}
			case "original-worktree":
				original.WorktreeID = strings.Repeat("1", 32)
			case "next-worktree":
				next.WorktreeID = strings.Repeat("1", 32)
			case "branch":
				next.Branch = "other"
			case "branch-identity":
				next.BranchID = "other-generation"
			case "invalid-code":
				next.GitCommit = "not-a-git-oid"
			}
			before, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			refBefore, err := f.store.GetRef(ctx, f.repo, domain.RefBranch, "main")
			if err != nil {
				t.Fatal(err)
			}
			historyBefore, err := f.store.ListHistoryEvents(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			err = f.store.CommitFrozenSnapshotIfCurrent(ctx, f.op.Ref, f.position.SharedTarget, original, next, &event)
			if !errors.Is(err, domain.ErrSelectionChanged) && !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("changed selection accepted or unexpected error: %v", err)
			}
			after, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejected job changed position", err)
			}
			refAfter, err := f.store.GetRef(ctx, f.repo, domain.RefBranch, "main")
			if err != nil || refAfter != refBefore {
				t.Fatal("rejected job changed shared ref", err)
			}
			historyAfter, err := f.store.ListHistoryEvents(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(historyBefore, historyAfter) {
				t.Fatal("rejected job recorded an advance", err)
			}
		})
	}
}
