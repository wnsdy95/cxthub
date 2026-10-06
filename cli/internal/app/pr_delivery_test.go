package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type prQueueRemote struct {
	outbound.RemoteSync
	fail    bool
	calls   int
	numbers []int
}

func (r *prQueueRemote) SubmitPRPromotion(ctx context.Context, repo string, pr domain.PullRequestMerge) error {
	r.calls++
	r.numbers = append(r.numbers, pr.Number)
	if r.fail {
		return errors.New("offline")
	}
	return nil
}
func TestPRDeliverySurvivesOfflineAndRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	repo := "sha256:" + strings.Repeat("1", 64)
	pr := domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	if err := st.QueuePRDelivery(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	remote := &prQueueRemote{fail: true}
	svc := newTestSyncService(st, remote, nil)
	if err := svc.flushPRDeliveries(ctx, repo); err != nil {
		t.Fatal(err)
	}
	st = storage.NewFileStore(root)
	pending, err := st.PendingPRDeliveries(ctx, repo)
	if err != nil || len(pending) != 1 {
		t.Fatalf("lost delivery: %+v %v", pending, err)
	}
	remote.fail = false
	svc = newTestSyncService(st, remote, nil)
	if err := svc.flushPRDeliveries(ctx, repo); err != nil {
		t.Fatal(err)
	}
	pending, err = st.PendingPRDeliveries(ctx, repo)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acceptance missing: %+v %v", pending, err)
	}
	if err := st.QueuePRDelivery(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingPRDeliveries(ctx, repo)
	if len(pending) != 0 {
		t.Fatal("duplicate delivery requeued")
	}
	pr.HeadSHA = strings.Repeat("c", 40)
	if err := st.QueuePRDelivery(ctx, repo, pr); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("changed PR accepted: %v", err)
	}
}

func TestPRDiscoveryHandoffDoesNotRequirePullOrSourcePublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	repo := "sha256:" + strings.Repeat("1", 64)
	remote := &prQueueRemote{fail: true}
	svc := newTestSyncService(st, remote, nil)
	// All RemoteSync methods except submission are nil: an accidental pull panics.
	for n := 1; n <= 25; n++ {
		pr := outbound.MergedPullRequest{Number: n, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeCommitSHA: strings.Repeat("b", 40)}
		if err := svc.QueuePullRequest(ctx, inbound.SyncInput{RepoID: repo}, pr); err != nil {
			t.Fatal(err)
		}
	}
	st = storage.NewFileStore(root)
	svc = newTestSyncService(st, remote, nil)
	pending, err := st.PendingPRDeliveries(ctx, repo)
	if err != nil || len(pending) != 25 {
		t.Fatalf("durable handoff: %d %v", len(pending), err)
	}
	remote.numbers = nil
	for i := 0; i < 2; i++ {
		if err := svc.flushPRDeliveries(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	for _, n := range remote.numbers {
		seen[n] = true
	}
	if len(seen) != 25 {
		t.Fatalf("failed deliveries starved later PRs: %v", remote.numbers)
	}
	remote.fail = false
	for i := 0; i < 2; i++ {
		if err := svc.flushPRDeliveries(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	pending, err = st.PendingPRDeliveries(ctx, repo)
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivery acknowledgement: %d %v", len(pending), err)
	}
}

type scopedPRSyncRemote struct {
	prQueueRemote
	refs   []domain.Ref
	pushed []domain.Ref
}

// Strict selected Push requires the existing protocol-1 identity/history contract.
func (r *scopedPRSyncRemote) ContextProtocol(context.Context, string) (int, error) { return 1, nil }
func (r *scopedPRSyncRemote) RegisterRepo(_ context.Context, repo domain.Repo) (domain.Repo, error) {
	return repo, nil
}
func (r *scopedPRSyncRemote) RemoteManifest(_ context.Context, repo string) (domain.Manifest, error) {
	return domain.Manifest{RepoID: repo, Refs: r.refs}, nil
}
func (r *scopedPRSyncRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return nil, nil
}
func (r *scopedPRSyncRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error { return nil }

func (r *scopedPRSyncRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return nil, nil, append([]domain.Ref{}, r.refs...), nil
}

func (r *scopedPRSyncRemote) Push(_ context.Context, _ string, _ []domain.Snapshot, _ []domain.SessionDoc, refs []domain.Ref, _, _ bool) error {
	r.pushed = append(r.pushed, refs...)
	return nil
}

func (r *scopedPRSyncRemote) DeleteUnsyncRemote(context.Context, string, string) error {
	return nil
}

func TestScopedSyncLeavesPRDeliveryQueuedUntilUnscopedSync(t *testing.T) {
	for _, operation := range []string{"pull", "missing-pull", "push"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st := storage.NewFileStore(root)
			repo := string(domain.HashContent([]byte(t.Name())))
			baseDoc, tipDoc := pullDoc(t, "base"), pullDoc(t, "selected branch tip")
			for _, doc := range []domain.SessionDoc{baseDoc, tipDoc} {
				if _, err := st.PutDoc(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			base := domain.Snapshot{ID: baseDoc.Hash, DocHash: baseDoc.Hash, RepoID: repo, Branch: "main"}
			tip := domain.Snapshot{ID: tipDoc.Hash, DocHash: tipDoc.Hash, RepoID: repo, Branch: "feature", Parents: []domain.ContentHash{base.ID}}
			for _, snap := range []domain.Snapshot{base, tip} {
				if err := st.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			main := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base.ID}
			feature := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: base.ID}
			if operation == "push" {
				feature.Target = tip.ID
			}
			for _, ref := range []domain.Ref{main, feature} {
				if err := st.PutRef(ctx, ref); err != nil {
					t.Fatal(err)
				}
			}
			// This deferred promotion targets main, outside the explicit feature scope.
			pr := domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "other-feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			if err := st.QueuePRDelivery(ctx, repo, pr); err != nil {
				t.Fatal(err)
			}
			remote := &scopedPRSyncRemote{refs: []domain.Ref{
				{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: tip.ID},
				{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: tip.ID},
			}}
			svc := newTestSyncService(st, remote, nil)
			in := inbound.SyncInput{RepoID: repo, Ref: "feature"}
			var out inbound.SyncOutput
			var err error
			if operation == "push" {
				in.Cwd = root
				svc.gitCtx = pushOrderGit{repo: domain.Repo{ID: repo, LocalPath: root}}
				out, err = svc.Push(ctx, in)
			} else {
				if operation == "missing-pull" {
					in.Ref = "missing"
				}
				out, err = svc.Pull(ctx, in)
			}
			if operation == "missing-pull" {
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("missing scoped ref: %v", err)
				}
			} else if err != nil || len(out.Conflicts) != 0 || len(out.NewRefs) != 1 || out.NewRefs[0].Name != "feature" || out.NewRefs[0].Target != tip.ID {
				t.Fatalf("scoped %s did not complete selected ref: %+v; %v", operation, out, err)
			}
			if operation == "push" && (len(remote.pushed) != 1 || remote.pushed[0].Name != "feature") {
				t.Fatalf("scoped push published unrelated refs: %+v", remote.pushed)
			}
			if remote.calls != 0 {
				t.Fatalf("scoped %s submitted unrelated PR delivery: %v", operation, remote.numbers)
			}
			// Reopen storage to prove that the retry obligation is still durable.
			st = storage.NewFileStore(root)
			pending, err := st.PendingPRDeliveries(ctx, repo)
			if err != nil || !reflect.DeepEqual(pending, []domain.PullRequestMerge{pr}) {
				t.Fatalf("scoped %s lost queued PR delivery: %+v; %v", operation, pending, err)
			}
			after, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || after.Target != main.Target {
				t.Fatalf("scoped %s changed unrelated main: %+v; %v", operation, after, err)
			}
			// The same public entry point must still drain the queue without a scope.
			svc = newTestSyncService(st, remote, nil)
			if operation == "push" {
				_, err = svc.Push(ctx, inbound.SyncInput{RepoID: repo})
			} else {
				_, err = svc.Pull(ctx, inbound.SyncInput{RepoID: repo})
			}
			if err != nil {
				t.Fatalf("unscoped sync: %v", err)
			}
			pending, err = storage.NewFileStore(root).PendingPRDeliveries(ctx, repo)
			if err != nil || len(pending) != 0 || remote.calls != 1 || !reflect.DeepEqual(remote.numbers, []int{pr.Number}) {
				t.Fatalf("unscoped sync did not accept queued PR exactly once: pending=%+v calls=%v; %v", pending, remote.numbers, err)
			}
		})
	}
}
