package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type failedPRJobFinishStore struct {
	*store.FSStore
	fail bool
}

func (s *failedPRJobFinishStore) FinishPRJob(ctx context.Context, j domain.PRPromotionJob) error {
	if s.fail && j.State == "completed" {
		return errors.New("job completion unavailable")
	}
	return s.FSStore.FinishPRJob(ctx, j)
}

func TestCompletedPRReplayPreservesLaterBasePosition(t *testing.T) {
	for _, unfinishedJob := range []bool{false, true} {
		for _, diverged := range []bool{false, true} {
			name := "completed-request"
			if unfinishedJob {
				name = "unfinished-job"
			}
			if diverged {
				name += "/new-continuation"
			} else {
				name += "/rewound"
			}
			t.Run(name, func(t *testing.T) {
				svc, st := newFsckSvc(t)
				ctx := context.Background()
				repo := hh(t.Name())
				if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/replay"}); err != nil {
					t.Fatal(err)
				}
				base := prSnapshot(t, st, repo, "base")
				source := prSnapshot(t, st, repo, "PR source", base)
				selected := base
				if diverged {
					selected = prSnapshot(t, st, repo, "intentional new continuation", base)
				}
				if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
					t.Fatal(err)
				}
				if err := svc.EnableContextProtocol(ctx, repo); err != nil {
					t.Fatal(err)
				}
				pr := domain.PullRequestMerge{Number: 175, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
				birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: pr.HeadBranch, Kind: "birth", Source: base, Target: source, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
				if err := svc.RecordHistory(ctx, birth); err != nil {
					t.Fatal(err)
				}
				publishPRSource(t, svc, birth)
				failing := &failedPRJobFinishStore{FSStore: st, fail: unfinishedJob}
				svc.meta = failing
				if _, err := svc.DeliverPRPromotion(ctx, repo, pr); (err != nil) != unfinishedJob {
					t.Fatalf("initial delivery: %v", err)
				}
				failing.fail = false
				rows, err := svc.ListHistory(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				completions := 0
				for _, e := range rows {
					if e.PRCompleted {
						completions++
					}
				}
				if completions != 1 {
					t.Fatalf("initial completions=%d", completions)
				}
				move := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "advance", Source: source, Target: selected, CreatedAt: time.Now().UTC()}
				if err := svc.RecordHistory(ctx, move); err != nil {
					t.Fatal(err)
				}
				beforeHistory, err := svc.ListHistory(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				beforeLog, err := st.ReadReflog(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				beforeSource, err := st.GetSnapshot(ctx, repo, source)
				if err != nil {
					t.Fatal(err)
				}
				var out inbound.UpdateRefOutput
				if unfinishedJob {
					claimed, claimErr := st.ClaimPRJob(ctx, repo, domain.PRPromotionID(repo, pr.Number), time.Now().Add(2*prJobLease), prJobLease)
					if claimErr != nil {
						t.Fatal(claimErr)
					}
					out, err = svc.runPRJob(ctx, claimed)
				} else {
					out, err = svc.DeliverPRPromotion(ctx, repo, pr)
				}
				if err != nil {
					t.Fatal(err)
				}
				current, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
				if err != nil || current.Target != selected {
					t.Fatalf("completed PR reapplied its ref effect: got=%s want=%s err=%v", current.Target, selected, err)
				}
				if out.Result != inbound.RefUpToDate || out.Ref.Target != selected || out.RequestedTarget != source {
					t.Fatalf("replay must report the current base: %+v", out)
				}
				afterHistory, err := svc.ListHistory(ctx, repo)
				if err != nil || !reflect.DeepEqual(beforeHistory, afterHistory) {
					t.Fatalf("replay changed immutable history: %v", err)
				}
				afterLog, err := st.ReadReflog(ctx, repo)
				if err != nil || !reflect.DeepEqual(beforeLog, afterLog) {
					t.Fatalf("replay moved a ref: %v", err)
				}
				afterSource, err := st.GetSnapshot(ctx, repo, source)
				if err != nil || !reflect.DeepEqual(beforeSource, afterSource) {
					t.Fatalf("replay changed the source graft: %v", err)
				}
				job, err := st.GetPRJob(ctx, repo, domain.PRPromotionID(repo, pr.Number))
				if err != nil || job.State != "completed" {
					t.Fatalf("replay did not complete delivery: %+v %v", job, err)
				}
			})
		}
	}
}
