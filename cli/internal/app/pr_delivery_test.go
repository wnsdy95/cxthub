package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type prQueueRemote struct {
	outbound.RemoteSync
	fail  bool
	calls int
}

func (r *prQueueRemote) SubmitPRPromotion(ctx context.Context, repo string, pr domain.PullRequestMerge) error {
	r.calls++
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
