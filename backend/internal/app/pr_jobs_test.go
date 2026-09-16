package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRJobSurvivesRestartAndLateSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := store.NewFSStore(dir)
	svc := NewService(st, st, nil, gitengine.NewEngine(st), st)
	repo := hh("durable-pr")
	_, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/job"})
	if err != nil {
		t.Fatal(err)
	}
	base := prSnapshot(t, st, repo, "base")
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	if _, err := svc.DeliverPRPromotion(ctx, repo, pr); !errors.Is(err, domain.ErrPRSourcePending) {
		t.Fatalf("late source=%v", err)
	}
	jobs, err := svc.ListPRPromotions(ctx, repo)
	if err != nil || len(jobs) != 1 || jobs[0].State != "waiting" {
		t.Fatalf("not durable: %+v %v", jobs, err)
	}
	st = store.NewFSStore(dir)
	svc = NewService(st, st, nil, gitengine.NewEngine(st), st)
	tip := prSnapshot(t, st, repo, "feature", base)
	e := domain.HistoryEvent{ID: strings.Repeat("4", 32), RepoID: string(repo), BranchID: "job-feature", Branch: "feature", Kind: "birth", Source: base, Target: tip, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, svc, e)
	if err := svc.RetryPRPromotion(ctx, repo, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessPRPromotions(ctx, 8); err != nil {
		t.Fatal(err)
	}
	jobs, _ = svc.ListPRPromotions(ctx, repo)
	if jobs[0].State != "completed" {
		t.Fatalf("state=%+v", jobs[0])
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.SubmitPRPromotion(ctx, repo, pr); err != nil {
			t.Fatal(err)
		}
		if err := svc.ProcessPRPromotions(ctx, 8); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := svc.ListHistory(ctx, repo)
	complete := 0
	for _, e := range events {
		if e.PRCompleted {
			complete++
		}
	}
	if complete != 1 {
		t.Fatalf("completions=%d", complete)
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != tip {
		t.Fatalf("ref=%+v %v", ref, err)
	}
	pr.HeadSHA = strings.Repeat("c", 40)
	if _, err := svc.SubmitPRPromotion(ctx, repo, pr); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tuple rewritten: %v", err)
	}
}

func TestQueuedPRFollowsBaseIdentityAcrossRenameAndReuse(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("queued-base-rename")
	_, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/rename"})
	if err != nil {
		t.Fatal(err)
	}
	base := prSnapshot(t, st, repo, "base")
	tip := prSnapshot(t, st, repo, "feature", base)
	unrelated := prSnapshot(t, st, repo, "new main", base)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 43, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	job, err := svc.SubmitPRPromotion(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	rename := domain.HistoryEvent{ID: strings.Repeat("5", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "trunk", PreviousBranch: "main", Kind: "rename", Source: base, Target: base, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, rename); err != nil {
		t.Fatal(err)
	}
	reused := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(repo), BranchID: "new-main", Branch: "main", Kind: "birth", BindingParent: rename.ID, Source: base, Target: unrelated, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, reused); err != nil {
		t.Fatal(err)
	}
	source := domain.HistoryEvent{ID: strings.Repeat("7", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: tip, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, source); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, svc, source)
	replay, err := svc.SubmitPRPromotion(ctx, repo, pr)
	if err != nil || replay.BaseBranchID != job.BaseBranchID {
		t.Fatalf("rebound queued job: %+v %v", replay, err)
	}
	if err := svc.ProcessPRPromotions(ctx, 8); err != nil {
		t.Fatal(err)
	}
	jobs, _ := svc.ListPRPromotions(ctx, repo)
	if jobs[0].State != "completed" {
		t.Fatalf("job=%+v", jobs[0])
	}
	for name, want := range map[string]domain.ContentHash{"trunk": tip, "main": unrelated} {
		ref, err := st.GetRef(ctx, repo, domain.RefBranch, name)
		if err != nil || ref.Target != want {
			t.Fatalf("%s=%+v %v", name, ref, err)
		}
	}
}
