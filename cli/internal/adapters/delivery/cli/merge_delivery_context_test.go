package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

type semanticMergeSync struct {
	*mergeDeliverySync
	view                domain.ContextQueryView
	err                 error
	onQuery             func()
	beforeAppend        func()
	afterAppend         func()
	conditionalAttempts int
}

func (syncer *semanticMergeSync) ResolveRemoteBranchContext(_ context.Context, in inbound.SyncInput, branch string) (domain.ContextQueryView, error) {
	if in.RepoID != syncer.ref.RepoID || in.ExpectedBranchID != syncer.ref.BranchID || branch != syncer.ref.Name {
		return domain.ContextQueryView{}, domain.ErrSelectionChanged
	}
	if syncer.onQuery != nil {
		syncer.onQuery()
	}
	return syncer.view, syncer.err
}

func (syncer *semanticMergeSync) AppendBranch(ctx context.Context, in inbound.SyncInput, branch string, target domain.ContentHash) error {
	syncer.conditionalAttempts++
	if syncer.beforeAppend != nil {
		syncer.beforeAppend()
	}
	if in.ExpectedTarget != syncer.view.Position || in.ExpectedRevision == nil || in.ExpectedRevision.Graph != syncer.view.Revision.Graph || in.ExpectedRevision.Evidence != syncer.view.Revision.Evidence {
		return domain.ErrSyncConflict
	}
	if err := syncer.mergeDeliverySync.AppendBranch(ctx, in, branch, target); err != nil {
		return err
	}
	if syncer.afterAppend != nil {
		syncer.afterAppend()
	}
	return nil
}

func semanticMergeView(syncer *mergeDeliverySync, candidates ...domain.ContentHash) domain.ContextQueryView {
	ids := append([]domain.ContentHash{syncer.ref.Target}, candidates...)
	snapshots := make([]domain.Snapshot, 0, len(ids))
	for _, id := range ids {
		snapshots = append(snapshots, domain.Snapshot{ID: id, RepoID: syncer.ref.RepoID})
	}
	return domain.ContextQueryView{
		Version: 1, StateHash: briefingHash("semantic inclusion"), Branch: syncer.ref.Name, Position: syncer.ref.Target,
		Snapshots: snapshots, Inclusion: &domain.BranchContext{BranchID: syncer.ref.BranchID, SnapshotID: syncer.ref.Target, SnapshotIDs: ids},
	}
}

func TestMergeDeliveryAcknowledgesSemanticInclusionWithoutAppending(t *testing.T) {
	for _, scenario := range []string{"included", "review", "excluded", "wrong branch identity", "wrong repository", "denied", "scope changed"} {
		t.Run(scenario, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			source, base := syncer.snapshots[0].ID, syncer.ref.Target
			syncer.appendErr = errors.New("offline")
			handleIncomingContexts(context.Background(), container, root)
			key, pending := mergeDeliveryJob(t, root)
			if len(pending.Candidates) != 1 || pending.Completed {
				t.Fatalf("not frozen: %+v", pending)
			}
			syncer.appendErr = nil
			semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer, source)}
			switch scenario {
			case "review", "excluded":
				semantic.view = semanticMergeView(syncer)
				semantic.view.Inclusion.Merges = []domain.BranchContextMerge{{Source: source, DestinationBranchID: syncer.ref.BranchID, State: scenario}}
			case "wrong branch identity":
				semantic.view.Inclusion.BranchID = "reused-main"
			case "wrong repository":
				semantic.view.Snapshots[1].RepoID = string(briefingHash("foreign"))
			case "denied":
				semantic.err = errors.New("access revoked")
			case "scope changed":
				semantic.onQuery = func() { container.History.(*mergeDeliveryHistory).binding.BranchID = "replacement-main" }
			}
			container.Sync = semantic
			replaySavedMergeDeliveries(context.Background(), container, root)
			job, err := readMergeDelivery(root, key)
			if err != nil {
				t.Fatal(err)
			}
			if job.Completed != (scenario == "included") {
				t.Fatalf("completion=%v scenario=%s reason=%s", job.Completed, scenario, job.Reason)
			}
			if len(syncer.appends) != 1 || syncer.ref.Target != base || snapshotContains(syncer.snapshots, base, source) {
				t.Fatalf("semantic membership rewrote raw DAG: appends=%v ref=%+v", syncer.appends, syncer.ref)
			}
		})
	}
}

func TestMergeDeliveryLateSourceAcceptsServerContributionAfterSuccessor(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	older := syncer.snapshots[0]
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	key, _ := mergeDeliveryJob(t, root)
	newer := completeMergeDeliverySuccessor(t, root, container, syncer)
	syncer.snapshots = append(syncer.snapshots, older)
	replaySavedMergeDeliveries(context.Background(), container, root)
	job, err := readMergeDelivery(root, key)
	if err != nil || !job.ReviewRequired || job.Completed {
		t.Fatalf("late safeguard missing: %+v err=%v", job, err)
	}
	container.Sync = &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer, older.ID)}
	replaySavedMergeDeliveries(context.Background(), container, root)
	job, err = readMergeDelivery(root, key)
	if err != nil || !job.Completed || job.ReviewRequired || len(syncer.appends) != 1 || syncer.ref.Target != newer || snapshotContains(syncer.snapshots, newer, older.ID) {
		t.Fatalf("server historical contribution not acknowledged without append: %+v err=%v", job, err)
	}
}

func TestMergeDeliveryFencesContributionChangeBetweenReadAndAppend(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	source, base := syncer.snapshots[0].ID, syncer.ref.Target
	semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
	semantic.view.Inclusion.CodeCommit = gitOut(root, "rev-parse", "HEAD")
	semantic.view.Revision = domain.RepositoryRevision{Graph: 7, Evidence: 9}
	semantic.beforeAppend = func() { semantic.view.Revision.Evidence++ }
	container.Sync = semantic
	handleIncomingContexts(context.Background(), container, root)
	key, pending := mergeDeliveryJob(t, root)
	if pending.Completed || semantic.conditionalAttempts != 1 || len(syncer.appends) != 0 || syncer.ref.Target != base {
		t.Fatalf("stale evidence appended: %+v attempts=%d appends=%v", pending, semantic.conditionalAttempts, syncer.appends)
	}
	semantic.beforeAppend = nil
	semantic.view = semanticMergeView(syncer, source)
	semantic.view.Revision = domain.RepositoryRevision{Graph: 7, Evidence: 10}
	replaySavedMergeDeliveries(context.Background(), container, root)
	job, err := readMergeDelivery(root, key)
	if err != nil || !job.Completed || semantic.conditionalAttempts != 1 || len(syncer.appends) != 0 || syncer.ref.Target != base {
		t.Fatalf("retry did not acknowledge semantic contribution: %+v err=%v", job, err)
	}
}

func TestMergeDeliveryRechecksSelectedInclusionBetweenCandidates(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	first := syncer.snapshots[0].ID
	runGitForTest(t, root, "commit", "--allow-empty", "-m", "second merged commit")
	second := briefingHash("second context")
	syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: second, RepoID: syncer.ref.RepoID, Message: "source [git " + gitOut(root, "rev-parse", "HEAD") + "]"})
	semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
	semantic.view.Inclusion.CodeCommit = gitOut(root, "rev-parse", "HEAD")
	semantic.view.Revision = domain.RepositoryRevision{Graph: 7, Evidence: 9}
	semantic.afterAppend = func() {
		semantic.view = semanticMergeView(syncer)
		semantic.view.Revision = domain.RepositoryRevision{Graph: 8, Evidence: 10}
		semantic.view.Inclusion.Merges = []domain.BranchContextMerge{{Source: second, DestinationBranchID: syncer.ref.BranchID, State: "review"}}
	}
	container.Sync = semantic
	handleIncomingContexts(context.Background(), container, root)
	_, job := mergeDeliveryJob(t, root)
	if len(job.Candidates) != 2 || job.Completed || len(syncer.appends) != 1 || syncer.appends[0] != first || syncer.ref.Target != first {
		t.Fatalf("later candidate bypassed new server assessment: job=%+v appends=%v", job, syncer.appends)
	}
}

func TestMergeDeliveryHistoricalAcknowledgementKeepsCurrentSelection(t *testing.T) {
	for _, scenario := range []string{"reset", "unverified", "other source", "other destination", "other merge", "abbreviated merge"} {
		t.Run(scenario, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			source, base := syncer.snapshots[0].ID, syncer.ref.Target
			syncer.appendErr = errors.New("lost acknowledgement")
			handleIncomingContexts(context.Background(), container, root)
			key, pending := mergeDeliveryJob(t, root)
			if pending.Completed || len(pending.Candidates) != 1 || len(pending.Evidence.SHAs) == 0 {
				t.Fatalf("missing frozen obligation: %+v", pending)
			}
			receipt := domain.BranchContextMerge{
				Source: source, DestinationBranchID: syncer.ref.BranchID,
				MergeSHA: pending.Evidence.SHAs[0], State: "not_selected", DeliveryAcknowledged: true,
			}
			switch scenario {
			case "unverified":
				receipt.DeliveryAcknowledged, receipt.State = false, "review"
			case "other source":
				receipt.Source = briefingHash("different source")
			case "other destination":
				receipt.DestinationBranchID = "inherited-branch"
			case "other merge":
				receipt.MergeSHA = pending.Evidence.Before
			case "abbreviated merge":
				receipt.MergeSHA = receipt.MergeSHA[:7]
			}
			semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
			semantic.view.Inclusion.Merges = []domain.BranchContextMerge{receipt}
			if scenario == "other source" {
				semantic.view.Inclusion.Merges = append(semantic.view.Inclusion.Merges, domain.BranchContextMerge{Source: source, State: "review"})
			}
			container.Sync = semantic
			_, reflected, attempted := replayMergeDeliveries(context.Background(), container, root)
			job, err := readMergeDelivery(root, key)
			if err != nil || !attempted || reflected || job.Completed != (scenario == "reset") {
				t.Fatalf("acknowledgement: job=%+v attempted=%v reflected=%v err=%v", job, attempted, reflected, err)
			}
			if semantic.conditionalAttempts != 0 || len(syncer.appends) != 1 || syncer.ref.Target != base {
				t.Fatalf("historical receipt changed selection: attempts=%d appends=%v ref=%+v", semantic.conditionalAttempts, syncer.appends, syncer.ref)
			}
		})
	}
}

func TestMergeDeliveryHistoricalAcknowledgementDoesNotDischargeMixedRange(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	first := syncer.snapshots[0].ID
	runGitForTest(t, root, "commit", "--allow-empty", "-m", "unrelated incoming commit")
	second := briefingHash("unresolved unrelated source")
	syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: second, RepoID: syncer.ref.RepoID, Message: "source [git " + gitOut(root, "rev-parse", "HEAD") + "]"})
	syncer.appendErr = errors.New("offline")
	handleIncomingContexts(context.Background(), container, root)
	key, pending := mergeDeliveryJob(t, root)
	if len(pending.Candidates) != 2 || pending.Completed {
		t.Fatalf("range not frozen: %+v", pending)
	}
	semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
	semantic.view.Inclusion.Merges = []domain.BranchContextMerge{
		{Source: first, DestinationBranchID: syncer.ref.BranchID, MergeSHA: pending.Evidence.SHAs[0], State: "not_selected", DeliveryAcknowledged: true},
		{Source: second, DestinationBranchID: syncer.ref.BranchID, State: "review"},
	}
	container.Sync = semantic
	replaySavedMergeDeliveries(context.Background(), container, root)
	job, err := readMergeDelivery(root, key)
	if err != nil || job.Completed || semantic.conditionalAttempts != 0 {
		t.Fatalf("receipt discharged unrelated source: job=%+v attempts=%d err=%v", job, semantic.conditionalAttempts, err)
	}
}

func TestMergeDeliveryHistoricalReceiptDoesNotProveEmptyRangeExhausted(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	source, base := syncer.snapshots[0].ID, syncer.ref.Target
	syncer.snapshots[0].Message = "original source before remote rewrite"
	semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
	semantic.view.Inclusion.Merges = []domain.BranchContextMerge{{
		Source: source, DestinationBranchID: syncer.ref.BranchID, MergeSHA: gitOut(root, "rev-parse", "HEAD"),
		State: "not_selected", DeliveryAcknowledged: true,
	}}
	container.Sync = semantic
	handleIncomingContexts(context.Background(), container, root)
	_, job := mergeDeliveryJob(t, root)
	if job.Completed || !job.WaitingSource || len(job.Candidates) != 0 || len(syncer.appends) != 0 || syncer.ref.Target != base {
		t.Fatalf("one receipt discharged an unenumerated range: %+v appends=%v ref=%+v", job, syncer.appends, syncer.ref)
	}
}
