package app

import (
	"context"
	"errors"
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
