package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestCodeSelectionUsesCompletedPRSourceAtMergeRevision(t *testing.T) {
	cwd, _, store, repo, baseline := historyFixture(t)
	before := gitOut(cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "merged code")
	merged := gitOut(cwd, "rev-parse", "HEAD")
	source := domain.HashContent([]byte("finalized source"))
	later := domain.HashContent([]byte("unrelated later main"))
	snaps, _ := store.ListSnapshots(context.Background(), repo, "")
	snaps = append(snaps, domain.Snapshot{ID: source, RepoID: repo, Parents: []domain.ContentHash{baseline}})
	old := domain.HistoryEvent{Branch: "main", BranchID: domain.LegacyContextBranchID(repo, "main"), Kind: "position", Target: baseline, GitAfter: before, CreatedAt: time.Unix(1, 0)}
	receipt := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: old.BranchID, Kind: "pr-merge", SourceBranchID: "feature", Source: source, Target: later, PRCompleted: true, PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: merged}, CreatedAt: time.Unix(2, 0)}
	for _, completed := range []bool{false, true} {
		receipt.PRCompleted = completed
		got := contextSelectionAtCode(cwd, merged, "main", snaps, []domain.HistoryEvent{old, receipt})
		want := baseline
		if completed {
			want = source
		}
		if got.Snapshot != want {
			t.Fatalf("completed=%v selected %s, want %s", completed, got.Snapshot, want)
		}
	}
	// The synthetic fallback selection can have a later wall clock than the
	// remote completion. It must not hide the exact merge association.
	old.GitAfter, old.CreatedAt = merged, time.Unix(10, 0)
	got := contextSelectionAtCode(cwd, merged, "main", snaps, []domain.HistoryEvent{old, receipt})
	if got.Snapshot != source {
		t.Fatalf("fallback hid completed source: %+v", got)
	}
	// A real later capture containing the source remains the most complete
	// observed context at this same code revision.
	snaps = append(snaps, domain.Snapshot{ID: later, RepoID: repo, Parents: []domain.ContentHash{source}})
	old.Target = later
	got = contextSelectionAtCode(cwd, merged, "main", snaps, []domain.HistoryEvent{old, receipt})
	if got.Snapshot != later {
		t.Fatalf("completion replaced a later capture: %+v", got)
	}
	// Leaving and returning to M must honor an explicit selection of A at M.
	old.Target, old.GitBefore = baseline, merged
	got = contextSelectionAtCode(cwd, merged, "main", snaps, []domain.HistoryEvent{old, receipt})
	if got.Snapshot != baseline {
		t.Fatalf("completion erased explicit historical selection: %+v", got)
	}
	// Legacy captures still provide same-code evidence when ordinary history
	// is absent. A receipt must not hide a later capture containing its source.
	old.GitAfter, old.GitBefore = before, ""
	snaps[len(snaps)-1].Branch = "main"
	snaps[len(snaps)-1].Message = "legacy capture [git " + merged + "]"
	got = contextSelectionAtCode(cwd, merged, "main", snaps, []domain.HistoryEvent{old, receipt})
	if got.Snapshot != later {
		t.Fatalf("completion hid legacy same-code capture: %+v", got)
	}
}

func TestCompletedPRPositionRefreshPreservesOtherSelections(t *testing.T) {
	for _, scenario := range []string{"incoming", "incomplete", "same-code-selection", "newer-capture", "already-current-baseline", "backward-reset", "detached", "future-ref", "different-identity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			cwd, c, plain, repo, baseline := historyFixture(t)
			before := gitOut(cwd, "rev-parse", "HEAD")
			gitDir := gitOut(cwd, "rev-parse", "--absolute-git-dir")
			st := storage.NewWorktreeFileStore(cwd, gitDir, "main", before)
			c.List, c.History = app.NewListSessionsService(st), app.NewContextHistoryService(st, st)
			if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: before, Snapshot: baseline, MemoryPinned: true}); err != nil {
				t.Fatal(err)
			}
			oldRef := publicationSnapshot(t, plain, repo, "main before promotion", baseline)
			if err := plain.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: oldRef}); err != nil {
				t.Fatal(err)
			}
			runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "merged code")
			merged := gitOut(cwd, "rev-parse", "HEAD")
			st = storage.NewWorktreeFileStore(cwd, gitDir, "main", merged)
			c.List, c.History = app.NewListSessionsService(st), app.NewContextHistoryService(st, st)
			if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: merged, Snapshot: baseline, MemoryPinned: true}); err != nil {
				t.Fatal(err)
			}
			source := publicationSnapshot(t, plain, repo, "finalized source", oldRef)
			branchID := domain.LegacyContextBranchID(repo, "main")
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: branchID, Target: source}
			receipt := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: branchID, Kind: "pr-merge", SourceBranchID: "feature", Source: source, Target: source, PRCompleted: true, PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: merged}, CreatedAt: time.Unix(2, 0)}
			proof := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: repo, Branch: "feature", BranchID: "feature", Kind: "position", Source: source, Target: source, MemoryPinned: true, GitAfter: receipt.PR.HeadSHA, CreatedAt: time.Unix(1, 0)}
			old, err := c.History.CurrentPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "incomplete":
				receipt.PRCompleted = false
			case "same-code-selection":
				old.Selection.GitBefore = merged
			case "newer-capture":
				old.Selection = nil
			case "already-current-baseline":
				old.SharedTarget = source
			case "backward-reset":
				runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "future code")
				old.Selection.GitBefore = gitOut(cwd, "rev-parse", "HEAD")
				runLifecycleGit(t, cwd, "reset", "--hard", merged)
			case "detached":
				runLifecycleGit(t, cwd, "switch", "--detach", merged)
			case "future-ref":
				ref.Target = publicationSnapshot(t, plain, repo, "newer main", source)
			case "different-identity":
				receipt.BranchID = "another-main"
			}
			if old.Selection != nil {
				old.Selection.ID = strings.Repeat("3", 32)
			}
			if err := st.PutWorkingPosition(ctx, old); err != nil {
				t.Fatal(err)
			}
			old, _ = c.History.CurrentPosition(ctx)
			for _, event := range []domain.HistoryEvent{proof, receipt} {
				if err := plain.PutHistoryEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if err := plain.PutRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			if err := reconcileCompletedPRPosition(ctx, c, cwd); err != nil {
				t.Fatal(err)
			}
			got, err := c.History.CurrentPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "incoming" {
				if got.Snapshot != source || got.SharedTarget != source || got.Rewound || !got.MemoryPinned || got.MemoryHash != "" {
					t.Fatalf("stale position after promotion: %+v", got)
				}
				if err := reconcileCompletedPRPosition(ctx, c, cwd); err != nil {
					t.Fatal(err)
				}
				again, _ := c.History.CurrentPosition(ctx)
				if !reflect.DeepEqual(got, again) {
					t.Fatal("duplicate refresh changed selection")
				}
			} else if !reflect.DeepEqual(got, old) {
				t.Fatalf("changed protected position:\nbefore=%+v\nafter=%+v", old, got)
			}
			actualRef, _ := plain.GetRef(ctx, repo, domain.RefBranch, "main")
			if actualRef.Target != ref.Target {
				t.Fatal("position refresh moved shared ref")
			}
		})
	}
}

func TestCompletedPRLegacyLabelCannotSelectMutableMemory(t *testing.T) {
	ctx := context.Background()
	cwd, c, st, repo, baseline := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	source := publicationSnapshot(t, st, repo, "legacy PR source", baseline)
	pinned, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: source, Summary: "recorded memory"})
	if err != nil {
		t.Fatal(err)
	}
	mutable, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: source, Summary: "later mutable memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapSnapshotMemory(ctx, source, "", mutable); err != nil {
		t.Fatal(err)
	}
	receipt := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: domain.LegacyContextBranchID(repo, "main"), Kind: "pr-merge", SourceBranchID: "feature", Source: source, Target: source, PRCompleted: true, PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: code}, CreatedAt: time.Unix(2, 0)}
	proof := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: repo, Branch: "feature", BranchID: "feature", Kind: "position", Source: source, Target: source, MemoryHash: pinned, MemoryPinned: true, GitAfter: receipt.PR.HeadSHA, CreatedAt: time.Unix(1, 0)}
	for _, e := range []domain.HistoryEvent{proof, receipt} {
		if err := st.PutHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err := st.ListSnapshots(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range snaps {
		if snaps[i].ID == source {
			snaps[i].Message = "legacy [git " + code + "]"
		}
	}
	history := []domain.HistoryEvent{proof, receipt}
	selected := contextSelectionAtCode(cwd, code, "main", snaps, history)
	selected, err = resolveCompletedPRMemory(ctx, c, "main", receipt.BranchID, selected, history)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Snapshot != source || selected.MemoryHash != pinned || !selected.MemoryPinned {
		t.Fatalf("legacy label replaced pinned source memory: %+v", selected)
	}
}
