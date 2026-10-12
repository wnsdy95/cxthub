package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

type mergeDeliveryHistory struct {
	inbound.ContextHistory
	binding domain.LocalBranchBinding
}

func (history *mergeDeliveryHistory) ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error) {
	return history.binding, nil
}

func (history *mergeDeliveryHistory) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	return domain.WorkingPosition{}, domain.ErrNotFound
}

type mergeDeliverySync struct {
	inbound.SyncRepo
	ref        domain.Ref
	snapshots  []domain.Snapshot
	pullErr    error
	appendErr  error
	appends    []domain.ContentHash
	onPull     func()
	lostReply  bool
	unverified bool
	onObserve  func()
	observeErr error
}

func (syncer *mergeDeliverySync) Pull(context.Context, inbound.SyncInput) (inbound.SyncOutput, error) {
	if syncer.onPull != nil {
		syncer.onPull()
	}
	return inbound.SyncOutput{}, syncer.pullErr
}

func (syncer *mergeDeliverySync) List(context.Context, inbound.ListInput) (inbound.ListOutput, error) {
	return inbound.ListOutput{Snapshots: syncer.snapshots, Refs: []domain.Ref{syncer.ref}}, nil
}

func (syncer *mergeDeliverySync) ResolveRemoteBranch(context.Context, inbound.SyncInput, string) (domain.Ref, error) {
	return syncer.ref, nil
}

func (syncer *mergeDeliverySync) ResolveRemoteBranchObservation(context.Context, inbound.SyncInput, string) (inbound.RemoteBranchObservation, error) {
	if syncer.onObserve != nil {
		syncer.onObserve()
	}
	return inbound.RemoteBranchObservation{Ref: syncer.ref, Snapshots: syncer.snapshots}, syncer.observeErr
}

func (syncer *mergeDeliverySync) AppendBranch(_ context.Context, _ inbound.SyncInput, _ string, target domain.ContentHash) error {
	syncer.appends = append(syncer.appends, target)
	if syncer.appendErr != nil {
		return syncer.appendErr
	}
	for index := range syncer.snapshots {
		if syncer.snapshots[index].ID == target {
			syncer.snapshots[index].GraftParents = []domain.ContentHash{syncer.ref.Target}
		}
	}
	syncer.ref.Target = target
	if syncer.unverified {
		syncer.ref.Target = briefingHash("merge delivery base")
	}
	if syncer.lostReply {
		return context.DeadlineExceeded
	}
	return nil
}

func newMergeDeliveryFixture(t *testing.T) (string, *Container, *mergeDeliverySync) {
	t.Helper()
	root := newPostMergeBriefingRepo(t)
	repo := domain.Repo{ID: string(briefingHash("merge delivery repo")), RemoteURL: "https://cxthub.test/acme/project", LocalPath: root}
	base := briefingHash("merge delivery base")
	target := briefingHash("merge delivery source")
	syncer := &mergeDeliverySync{
		ref:       domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: "main-identity", Target: base},
		snapshots: []domain.Snapshot{{ID: target, RepoID: repo.ID, Message: fmt.Sprintf("source [git %s]", gitOut(root, "rev-parse", "HEAD"))}, {ID: base, RepoID: repo.ID}},
	}
	container := &Container{List: syncer, Sync: syncer,
		ResolveRepo: func(context.Context, string) (domain.Repo, error) { return repo, nil },
		History:     &mergeDeliveryHistory{binding: domain.LocalBranchBinding{LocalBranch: "main", Branch: "main", BranchID: "main-identity"}},
	}
	return root, container, syncer
}

func TestMergeDeliveryRetainsOriginalRangeAfterFailedFetch(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.pullErr = context.DeadlineExceeded
	handleIncomingContexts(context.Background(), container, root)
	previousHead := gitOut(root, "rev-parse", "HEAD")
	runGitForTest(t, root, "commit", "--allow-empty", "-m", "later unrelated merge")
	runGitForTest(t, root, "update-ref", "ORIG_HEAD", previousHead)
	syncer.pullErr = nil
	handleIncomingContexts(context.Background(), container, root)
	if len(syncer.appends) != 1 || syncer.appends[0] != briefingHash("merge delivery source") {
		t.Fatalf("lost original merge candidate after ORIG_HEAD changed: %v", syncer.appends)
	}
	handleIncomingContexts(context.Background(), container, root)
	if len(syncer.appends) != 1 {
		t.Fatalf("completed merge appended again: %v", syncer.appends)
	}
}

func TestMergeDeliveryRetainsFailedAppendAfterGitRangeDisappears(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("remote unavailable")
	handleIncomingContexts(context.Background(), container, root)
	runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
	syncer.appendErr = nil
	handleIncomingContexts(context.Background(), container, root)
	if len(syncer.appends) != 2 || syncer.ref.Target != briefingHash("merge delivery source") {
		t.Fatalf("lost failed append on retry: %v", syncer.appends)
	}
}

func mergeDeliveryJob(t *testing.T, root string) (string, mergeDelivery) {
	t.Helper()
	jobs, err := listMergeDeliveries(root)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	for key, job := range jobs {
		return key, job
	}
	panic("unreachable")
}

func TestMergeDeliveryFrozenCandidatesAndLostReply(t *testing.T) {
	for _, mode := range []string{"failed_append", "lost_reply", "unverified_success"} {
		t.Run(mode, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			syncer.appendErr = errors.New("temporary failure")
			if mode != "failed_append" {
				syncer.appendErr = nil
			}
			syncer.lostReply, syncer.unverified = mode == "lost_reply", mode == "unverified_success"
			handleIncomingContexts(context.Background(), container, root)
			_, job := mergeDeliveryJob(t, root)
			if job.Completed || len(job.Candidates) != 1 || job.Reason == "" || job.Lease != "" {
				t.Fatalf("unsafe completion: %+v", job)
			}
			syncer.snapshots = append([]domain.Snapshot{{ID: briefingHash("later replacement"), Message: syncer.snapshots[0].Message}}, syncer.snapshots...)
			syncer.appendErr, syncer.lostReply, syncer.unverified = nil, false, false
			runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
			replaySavedMergeDeliveries(context.Background(), container, root)
			_, job = mergeDeliveryJob(t, root)
			if !job.Completed || syncer.ref.Target != briefingHash("merge delivery source") {
				t.Fatalf("frozen candidate not completed: %+v", job)
			}
			want := 2
			if mode == "lost_reply" {
				want = 1
			}
			if len(syncer.appends) != want {
				t.Fatalf("appends=%v want=%d", syncer.appends, want)
			}
		})
	}
}

func TestMergeDeliveryScopeAndCorruptionFailClosed(t *testing.T) {
	for _, mode := range []string{"origin", "context_endpoint", "repository", "branch_identity", "reset", "corrupt_evidence", "corrupt_plan"} {
		t.Run(mode, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			syncer.appendErr = errors.New("temporary failure")
			handleIncomingContexts(context.Background(), container, root)
			key, job := mergeDeliveryJob(t, root)
			syncer.appendErr, syncer.appends = nil, nil
			switch mode {
			case "origin":
				runGitForTest(t, root, "remote", "set-url", "origin", "https://example.test/other.git")
			case "context_endpoint":
				t.Setenv("CXT_REMOTE", "https://different.test/api/v1")
			case "repository":
				container.ResolveRepo = func(context.Context, string) (domain.Repo, error) {
					return domain.Repo{ID: string(briefingHash("other repo"))}, nil
				}
			case "branch_identity":
				container.History.(*mergeDeliveryHistory).binding.BranchID = "recreated-main"
			case "reset":
				runGitForTest(t, root, "reset", "--hard", "ORIG_HEAD")
			case "corrupt_evidence":
				job.Evidence.SHAs[0] = strings.Repeat("a", 40)
				if err := writeMergeDelivery(root, key, job); err != nil {
					t.Fatal(err)
				}
			case "corrupt_plan":
				job.Candidates[0] = briefingHash("tampered candidate")
				if err := writeMergeDelivery(root, key, job); err != nil {
					t.Fatal(err)
				}
			}
			replaySavedMergeDeliveries(context.Background(), container, root)
			if len(syncer.appends) != 0 {
				t.Fatalf("unsafe scope replay: %v", syncer.appends)
			}
			if _, err := os.Stat(filepath.Join(root, mergeDeliveryPath(key))); err != nil {
				t.Fatal("evidence was removed", err)
			}
		})
	}
}

func TestMergeDeliveryConcurrentClaimAndLeaseRecovery(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	syncer.onPull = func() { close(entered); <-release }
	go func() { replayMergeDeliveries(context.Background(), container, root); close(done) }()
	<-entered
	if _, _, attempted := replayMergeDeliveries(context.Background(), container, root); attempted {
		t.Fatal("concurrent worker stole live lease")
	}
	close(release)
	<-done
	if len(syncer.appends) != 1 {
		t.Fatalf("concurrent append count=%d", len(syncer.appends))
	}
	key, job := mergeDeliveryJob(t, root)
	job.Completed, job.Lease, job.LeaseUntil = false, "dead-process", time.Now().Add(-time.Minute)
	if err := writeMergeDelivery(root, key, job); err != nil {
		t.Fatal(err)
	}
	syncer.onPull = nil
	replayMergeDeliveries(context.Background(), container, root)
	_, job = mergeDeliveryJob(t, root)
	if !job.Completed || len(syncer.appends) != 1 {
		t.Fatalf("lease recovery repeated append: %+v", job)
	}
	if err := updateLeasedMerge(context.Background(), root, key, "dead-process", func(saved *mergeDelivery) { saved.Completed = false }); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale lease write: %v", err)
	}
}

type mergeDeliveryDiscovery struct {
	*mergeDeliverySync
	supported bool
}

func (syncer *mergeDeliveryDiscovery) DiscoverIncomingSnapshots(context.Context, inbound.SyncInput) ([]domain.Snapshot, bool, error) {
	return syncer.snapshots, syncer.supported, nil
}

func TestMergeDeliveryRetainsCapabilityAcrossFailedHydration(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	discovery := &mergeDeliveryDiscovery{mergeDeliverySync: syncer, supported: true}
	container.Sync = discovery
	syncer.pullErr = context.DeadlineExceeded
	handleIncomingContexts(context.Background(), container, root)
	_, job := mergeDeliveryJob(t, root)
	if !job.RequireBranchPlan || job.Completed {
		t.Fatal("lost negotiated scope")
	}
	discovery.supported, syncer.pullErr = false, nil
	replaySavedMergeDeliveries(context.Background(), container, root)
	_, job = mergeDeliveryJob(t, root)
	if len(syncer.appends) != 0 || job.Completed || !strings.Contains(job.Reason, "branch-plan") {
		t.Fatalf("capability downgrade: %+v", job)
	}
}

func TestMergeDeliveryRetriesLateSourceAndFreezesRewriteMapping(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	original := syncer.snapshots[0]
	oldSHA := strings.Repeat("a", 40)
	if err := saveRewrites(root, map[string]string{oldSHA: gitOut(root, "rev-parse", "HEAD")}); err != nil {
		t.Fatal(err)
	}
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	_, job := mergeDeliveryJob(t, root)
	if job.Completed || len(job.Candidates) != 0 {
		t.Fatal("missing source acknowledged")
	}
	if err := saveRewrites(root, map[string]string{oldSHA: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	original.Message = "original capture [git " + oldSHA + "]"
	syncer.snapshots = append([]domain.Snapshot{original}, syncer.snapshots...)
	replaySavedMergeDeliveries(context.Background(), container, root)
	_, job = mergeDeliveryJob(t, root)
	if !job.Completed || len(syncer.appends) != 1 || syncer.appends[0] != original.ID {
		t.Fatalf("late source or original rewrite lost: %+v", job)
	}
}

func TestMergeDeliveryChecksScopeAfterObservation(t *testing.T) {
	for _, mode := range []string{"reset", "branch_identity", "context_endpoint", "lease"} {
		t.Run(mode, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			syncer.onObserve = func() {
				syncer.onObserve = nil
				switch mode {
				case "reset":
					runGitForTest(t, root, "reset", "--hard", "ORIG_HEAD")
				case "branch_identity":
					container.History.(*mergeDeliveryHistory).binding.BranchID = "replacement"
				case "context_endpoint":
					t.Setenv("CXT_REMOTE", "https://replacement.test/api/v1")
				case "lease":
					key, job := mergeDeliveryJob(t, root)
					job.Lease = "another-worker"
					if err := writeMergeDelivery(root, key, job); err != nil {
						t.Fatal(err)
					}
				}
			}
			handleIncomingContexts(context.Background(), container, root)
			if len(syncer.appends) != 0 {
				t.Fatal("scope changed while observing but append ran")
			}
		})
	}
}

func TestMergeDeliveryFrozenRetryDoesNotHydrateReplacement(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("temporary append failure")
	handleIncomingContexts(context.Background(), container, root)
	syncer.snapshots = append([]domain.Snapshot{{ID: briefingHash("replacement"), Message: syncer.snapshots[0].Message}}, syncer.snapshots...)
	syncer.appendErr = nil
	syncer.onPull = func() { t.Fatal("frozen retry rediscovered/hydrated mutable replacement") }
	replaySavedMergeDeliveries(context.Background(), container, root)
	_, job := mergeDeliveryJob(t, root)
	if !job.Completed || syncer.ref.Target != briefingHash("merge delivery source") {
		t.Fatalf("frozen plan not completed: %+v", job)
	}
}

func TestMergeDeliveryPredecessorCannotBeOvertaken(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("first range failed")
	handleIncomingContexts(context.Background(), container, root)
	before := gitOut(root, "rev-parse", "HEAD")
	runGitForTest(t, root, "commit", "--allow-empty", "-m", "second range")
	runGitForTest(t, root, "update-ref", "ORIG_HEAD", before)
	newTarget := briefingHash("second merge source")
	syncer.snapshots = append([]domain.Snapshot{{ID: newTarget, Message: "second [git " + gitOut(root, "rev-parse", "HEAD") + "]"}}, syncer.snapshots...)
	handleIncomingContexts(context.Background(), container, root)
	if len(syncer.appends) != 2 || syncer.appends[1] == newTarget {
		t.Fatalf("successor overtook failed predecessor: %v", syncer.appends)
	}
	syncer.appendErr = nil
	replaySavedMergeDeliveries(context.Background(), container, root)
	replaySavedMergeDeliveries(context.Background(), container, root)
	if syncer.ref.Target != newTarget || len(syncer.appends) != 4 || syncer.appends[2] != briefingHash("merge delivery source") {
		t.Fatalf("out-of-order completion: %v", syncer.appends)
	}
}

func TestMergeDeliveryKeepsWholeRangeAndPrivateDestinations(t *testing.T) {
	root, container, _ := newMergeDeliveryFixture(t)
	runGitForTest(t, root, "remote", "set-url", "origin", "https://synthetic-user:synthetic-secret@example.test/repo")
	incoming := readIncomingGitRange(root)
	incoming.SHAs = nil
	for index := 1; index <= 800; index++ {
		incoming.SHAs = append(incoming.SHAs, fmt.Sprintf("%040x", index))
	}
	if err := persistMergeDelivery(context.Background(), container, root, incoming); err != nil {
		t.Fatal(err)
	}
	key, job := mergeDeliveryJob(t, root)
	if len(job.Evidence.SHAs) != 800 {
		t.Fatal("whole range was split or lost")
	}
	raw, err := os.ReadFile(filepath.Join(root, mergeDeliveryPath(key)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "https://") {
		t.Fatal("destination credentials leaked to journal")
	}
}

func TestMergeDeliveryLateHistoricalSourceDoesNotReplaceNewerTip(t *testing.T) {
	for _, failedDiscovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed_discovery=%t", failedDiscovery), func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			older := syncer.snapshots[0]
			syncer.snapshots = syncer.snapshots[1:]
			handleIncomingContexts(context.Background(), container, root)
			key, _ := mergeDeliveryJob(t, root)
			newer := completeMergeDeliverySuccessor(t, root, container, syncer)
			if failedDiscovery {
				syncer.pullErr = context.DeadlineExceeded
				replaySavedMergeDeliveries(context.Background(), container, root)
				syncer.pullErr = nil
			}
			syncer.snapshots = append(syncer.snapshots, older)
			for attempt := 0; attempt < 2; attempt++ {
				replaySavedMergeDeliveries(context.Background(), container, root)
			}
			if syncer.ref.Target != newer || len(syncer.appends) != 1 {
				t.Fatalf("late source became latest tip: %v", syncer.appends)
			}
			job, err := readMergeDelivery(root, key)
			if err != nil || job.Completed || !job.LateSource || !job.ReviewRequired || !strings.Contains(job.Reason, "historical insertion requires review") {
				t.Fatalf("missing durable review state: %+v err=%v", job, err)
			}
		})
	}
}

func queueMergeDeliveryCommit(t *testing.T, root string, container *Container, message string) string {
	t.Helper()
	before := gitOut(root, "rev-parse", "HEAD")
	runGitForTest(t, root, "commit", "--allow-empty", "-m", message)
	runGitForTest(t, root, "update-ref", "ORIG_HEAD", before)
	incoming := readIncomingGitRange(root)
	if err := persistMergeDelivery(context.Background(), container, root, incoming); err != nil {
		t.Fatal(err)
	}
	return incoming.After
}

func completeMergeDeliverySuccessor(t *testing.T, root string, container *Container, syncer *mergeDeliverySync) domain.ContentHash {
	t.Helper()
	code := queueMergeDeliveryCommit(t, root, container, "newer context merged")
	newer := briefingHash("already completed newer merge")
	syncer.snapshots = append([]domain.Snapshot{{ID: newer, Message: "newer [git " + code + "]"}}, syncer.snapshots...)
	replaySavedMergeDeliveries(context.Background(), container, root)
	if syncer.ref.Target != newer {
		t.Fatal("successor did not complete")
	}
	return newer
}

func TestMergeDeliveryCompletedSuccessorBlocksEveryAppend(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	incoming := readIncomingGitRange(root)
	older := syncer.snapshots[0]
	syncer.snapshots = syncer.snapshots[1:]
	newer := completeMergeDeliverySuccessor(t, root, container, syncer)
	syncer.snapshots = append(syncer.snapshots, older)
	if err := persistMergeDelivery(context.Background(), container, root, incoming); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		replaySavedMergeDeliveries(context.Background(), container, root)
	}
	jobs, err := listMergeDeliveries(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Evidence.After == incoming.After && (job.Completed || job.LateSource || !job.ReviewRequired || len(job.Candidates) != 1) {
			t.Fatalf("previously undiscovered range bypassed successor guard: %+v", job)
		}
	}
	if len(syncer.appends) != 1 || syncer.ref.Target != newer {
		t.Fatalf("historical range replaced completed successor: %v", syncer.appends)
	}
}

func TestMergeDeliveryFrozenPlanClearsWaitingBeforeAcknowledgement(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	older := syncer.snapshots[0]
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	key, job := mergeDeliveryJob(t, root)
	if !job.WaitingSource {
		t.Fatal("source was not waiting")
	}
	job.Lease, job.LeaseUntil = "interrupted-worker", time.Now().Add(time.Minute)
	if err := writeMergeDelivery(root, key, job); err != nil {
		t.Fatal(err)
	}
	syncer.snapshots = append(syncer.snapshots, older)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncer.onObserve = cancel
	if _, _, err := executeMergeDelivery(ctx, container, root, key, job.Lease, job); err == nil {
		t.Fatal("interrupted execution unexpectedly succeeded")
	}
	frozen, err := readMergeDelivery(root, key)
	if err != nil || frozen.Completed || frozen.WaitingSource || !frozen.LateSource || len(frozen.Candidates) != 1 || len(syncer.appends) != 0 {
		t.Fatalf("plan was not atomically frozen: %+v err=%v", frozen, err)
	}
	frozen.LeaseUntil = time.Now().Add(-time.Minute)
	if err := writeMergeDelivery(root, key, frozen); err != nil {
		t.Fatal(err)
	}
	syncer.onObserve = nil
	newerCode := queueMergeDeliveryCommit(t, root, container, "successor after interrupted plan")
	newer := briefingHash("successor after interrupted plan")
	syncer.snapshots = append([]domain.Snapshot{{ID: newer, Message: "newer [git " + newerCode + "]"}}, syncer.snapshots...)
	replaySavedMergeDeliveries(context.Background(), container, root)
	if len(syncer.appends) != 1 || syncer.appends[0] != older.ID {
		t.Fatalf("successor overtook frozen predecessor: %v", syncer.appends)
	}
	replaySavedMergeDeliveries(context.Background(), container, root)
	if syncer.ref.Target != newer || len(syncer.appends) != 2 {
		t.Fatalf("successor did not complete after predecessor: %v", syncer.appends)
	}
}

func TestMergeDeliveryNonAncestralJobDoesNotStarveWaitingSource(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("old append failed")
	handleIncomingContexts(context.Background(), container, root)
	key, abandoned := mergeDeliveryJob(t, root)
	runGitForTest(t, root, "reset", "--hard", "ORIG_HEAD")
	syncer.appendErr, syncer.appends = nil, nil
	code := queueMergeDeliveryCommit(t, root, container, "replacement code range")
	replaySavedMergeDeliveries(context.Background(), container, root)
	newer := briefingHash("source arriving after reset")
	syncer.snapshots = append([]domain.Snapshot{{ID: newer, Message: "newer [git " + code + "]"}}, syncer.snapshots...)
	replaySavedMergeDeliveries(context.Background(), container, root)
	if len(syncer.appends) != 1 || syncer.ref.Target != newer {
		t.Fatalf("discarded range starved current source: %v", syncer.appends)
	}
	after, err := readMergeDelivery(root, key)
	if err != nil || after.Completed || after.Attempts != abandoned.Attempts {
		t.Fatalf("discarded range was consumed or retried: %+v err=%v", after, err)
	}
	runGitForTest(t, root, "merge", "--no-edit", abandoned.Evidence.After)
	replaySavedMergeDeliveries(context.Background(), container, root)
	after, err = readMergeDelivery(root, key)
	if err != nil || !after.Completed || len(syncer.appends) != 2 || syncer.ref.Target != briefingHash("merge delivery source") {
		t.Fatalf("restored code range did not recover automatically: %+v appends=%v err=%v", after, syncer.appends, err)
	}
}

func TestMergeDeliveryReviewFailureDoesNotStarveWaitingSource(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	older := syncer.snapshots[0]
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	key, _ := mergeDeliveryJob(t, root)
	completeMergeDeliverySuccessor(t, root, container, syncer)
	syncer.snapshots = append(syncer.snapshots, older)
	replaySavedMergeDeliveries(context.Background(), container, root)
	code := queueMergeDeliveryCommit(t, root, container, "third range awaiting source")
	replaySavedMergeDeliveries(context.Background(), container, root)
	syncer.observeErr = context.DeadlineExceeded
	replaySavedMergeDeliveries(context.Background(), container, root)
	syncer.observeErr = nil
	job, err := readMergeDelivery(root, key)
	if err != nil || !job.ReviewRequired || job.Completed {
		t.Fatalf("observation failure cleared review state: %+v err=%v", job, err)
	}
	latest := briefingHash("third source")
	syncer.snapshots = append([]domain.Snapshot{{ID: latest, Message: "latest [git " + code + "]"}}, syncer.snapshots...)
	replaySavedMergeDeliveries(context.Background(), container, root)
	if syncer.ref.Target != latest || len(syncer.appends) != 2 {
		t.Fatalf("historical review starved current source: %v", syncer.appends)
	}
	job, err = readMergeDelivery(root, key)
	if err != nil || !job.ReviewRequired || job.Completed {
		t.Fatalf("historical source was falsely completed: %+v err=%v", job, err)
	}
}

func TestMergeDeliveryReviewAcknowledgesVerifiedInclusion(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	older := syncer.snapshots[0]
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	key, _ := mergeDeliveryJob(t, root)
	newer := completeMergeDeliverySuccessor(t, root, container, syncer)
	syncer.snapshots = append(syncer.snapshots, older)
	replaySavedMergeDeliveries(context.Background(), container, root)
	for index := range syncer.snapshots {
		if syncer.snapshots[index].ID == newer {
			syncer.snapshots[index].GraftParents = append(syncer.snapshots[index].GraftParents, older.ID)
		}
	}
	replaySavedMergeDeliveries(context.Background(), container, root)
	job, err := readMergeDelivery(root, key)
	if err != nil || !job.Completed || job.ReviewRequired || len(syncer.appends) != 1 || syncer.ref.Target != newer {
		t.Fatalf("verified inclusion was not acknowledged without a write: %+v appends=%v err=%v", job, syncer.appends, err)
	}
}

func TestMergeDeliveryRangeIdentityIgnoresRewriteChanges(t *testing.T) {
	for _, phase := range []string{"undiscovered", "planned", "completed"} {
		t.Run(phase, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			oldSHA := strings.Repeat("a", 40)
			rewrites := map[string]string{oldSHA: gitOut(root, "rev-parse", "HEAD")}
			if err := saveRewrites(root, rewrites); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "undiscovered":
				syncer.pullErr = context.DeadlineExceeded
			case "planned":
				syncer.appendErr = errors.New("append unavailable")
			}
			handleIncomingContexts(context.Background(), container, root)
			key, original := mergeDeliveryJob(t, root)
			before, err := os.ReadFile(filepath.Join(root, mergeDeliveryPath(key)))
			if err != nil {
				t.Fatal(err)
			}
			rewrites[strings.Repeat("b", 40)] = strings.Repeat("c", 40)
			if err := saveRewrites(root, rewrites); err != nil {
				t.Fatal(err)
			}
			changed := original.Evidence
			changed.Rewrites = rewrites
			if mergeDeliveryKey(changed) != key {
				t.Error("rewrite metadata changed the original range identity")
			}
			if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".cxt", "rewrites.json"), []byte("invalid mutable rewrites"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
				t.Fatalf("existing range depended on mutable rewrites: %v", err)
			}
			repeated, _ := mergeDeliveryJob(t, root)
			after, err := os.ReadFile(filepath.Join(root, mergeDeliveryPath(repeated)))
			if err != nil || repeated != key || !bytes.Equal(before, after) {
				t.Fatalf("repeated range replaced frozen evidence: key=%s err=%v", repeated, err)
			}
		})
	}
}

func TestMergeDeliveryRejectsCorruptFrozenRewrites(t *testing.T) {
	root, container, _ := newMergeDeliveryFixture(t)
	oldSHA := strings.Repeat("a", 40)
	if err := saveRewrites(root, map[string]string{oldSHA: gitOut(root, "rev-parse", "HEAD")}); err != nil {
		t.Fatal(err)
	}
	if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
		t.Fatal(err)
	}
	key, job := mergeDeliveryJob(t, root)
	job.Evidence.Rewrites[oldSHA] = strings.Repeat("b", 40)
	if err := writeMergeDelivery(root, key, job); err != nil {
		t.Fatal(err)
	}
	_, err := readMergeDelivery(root, key)
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt frozen rewrites accepted: %v", err)
	}
}
