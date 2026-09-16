package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func finalizationJobFixture(t *testing.T) (*Service, *store.FSStore, domain.PRPromotionJob, domain.HistoryEvent) {
	t.Helper()
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/finalization"}); err != nil {
		t.Fatal(err)
	}
	base := prSnapshot(t, st, repo, "base")
	source := prSnapshot(t, st, repo, "old source", base)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 175, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	proof := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: source, GitAfter: pr.HeadSHA, WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(100, 0).UTC()}
	if err := svc.RecordHistory(ctx, proof); err != nil {
		t.Fatal(err)
	}
	job, err := svc.SubmitPRPromotion(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, job, proof
}

func exhaustSourceAttempts(t *testing.T, svc *Service, st *store.FSStore, job domain.PRPromotionJob) {
	t.Helper()
	ctx := context.Background()
	for attempt := 1; attempt <= prSourcePendingAttempts; attempt++ {
		claimed, err := st.ClaimPRJob(ctx, job.RepoID, job.ID, time.Now().Add(time.Hour), prJobLease)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.runPRJob(ctx, claimed); !errors.Is(err, domain.ErrPRSourcePending) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		got, err := st.GetPRJob(ctx, job.RepoID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		state, reason := "waiting", "source_context_pending"
		if attempt == prSourcePendingAttempts {
			state, reason = "attention", "source_finalization_required"
		}
		if got.State != state || got.Reason != reason || got.Attempts != attempt {
			t.Fatalf("attempt %d: %+v", attempt, got)
		}
	}
}

func TestPRFinalizationAttentionAllowsLaterJobAndExactPublicationRecovery(t *testing.T) {
	ctx := context.Background()
	svc, st, old, proof := finalizationJobFixture(t)
	exhaustSourceAttempts(t, svc, st, old)
	rows, _ := svc.ListHistory(ctx, old.RepoID)
	for _, e := range rows {
		if e.Kind == "pr-merge" {
			t.Fatal("unfinalized legacy source acquired a receipt")
		}
	}
	newPR := old.PR
	newPR.Number, newPR.HeadSHA, newPR.MergeSHA = 176, strings.Repeat("c", 40), strings.Repeat("d", 40)
	newProof := proof
	newProof.ID, newProof.Kind, newProof.GitAfter = strings.Repeat("2", 32), "position", newPR.HeadSHA
	newProof.Target = prSnapshot(t, st, old.RepoID, "newer source", proof.Target)
	newProof.Source = newProof.Target
	if err := svc.RecordHistory(ctx, newProof); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, svc, newProof)
	oldState, _ := st.GetPRJob(ctx, old.RepoID, old.ID)
	if oldState.State != "attention" {
		t.Fatal("different Git revision woke old job")
	}
	newJob, err := svc.SubmitPRPromotion(ctx, old.RepoID, newPR)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessPRPromotions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetPRJob(ctx, old.RepoID, newJob.ID)
	if got.State != "completed" {
		t.Fatalf("legacy job still blocks ready successor: %+v", got)
	}
	publishPRSource(t, svc, proof)
	woken, _ := st.GetPRJob(ctx, old.RepoID, old.ID)
	if woken.State != "waiting" || woken.Attempts != 0 || woken.CreatedAt != old.CreatedAt {
		t.Fatalf("exact publication did not retain/requeue old job: %+v", woken)
	}
	if err := svc.ProcessPRPromotions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetPRJob(ctx, old.RepoID, old.ID)
	ref, _ := st.GetRef(ctx, old.RepoID, domain.RefBranch, "main")
	if got.State != "completed" || ref.Target != newProof.Target {
		t.Fatalf("late finalized source recovery lost successor: %+v %+v", got, ref)
	}
}

type publicationBeforeAttentionStore struct {
	*store.FSStore
	beforeFinish func()
}

func (s *publicationBeforeAttentionStore) FinishPRJob(ctx context.Context, j domain.PRPromotionJob) error {
	if j.Reason == "source_finalization_required" && s.beforeFinish != nil {
		fn := s.beforeFinish
		s.beforeFinish = nil
		fn()
	}
	return s.FSStore.FinishPRJob(ctx, j)
}

func TestPRFinalizationWakeSurvivesAttentionRaceAndRestart(t *testing.T) {
	for _, scenario := range []string{"publication before attention commit", "crash after publication commit", "acknowledged publication retry"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, st, job, proof := finalizationJobFixture(t)
			if scenario == "publication before attention commit" {
				wrapped := &publicationBeforeAttentionStore{FSStore: st}
				wrapped.beforeFinish = func() { publishPRSource(t, svc, proof) }
				svc.meta = wrapped
			}
			exhaustSourceAttempts(t, svc, st, job)
			if scenario != "publication before attention commit" {
				// Storage succeeded but the process died before the service wake.
				if err := st.ApplyHistoryEvent(ctx, prPublication(proof)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "acknowledged publication retry" {
				publishPRSource(t, svc, proof)
				got, _ := st.GetPRJob(ctx, job.RepoID, job.ID)
				if got.State != "waiting" {
					t.Fatalf("idempotent publication retry did not wake: %+v", got)
				}
			}
			if err := svc.ProcessPRPromotions(ctx, 1); err != nil {
				t.Fatal(err)
			}
			got, _ := st.GetPRJob(ctx, job.RepoID, job.ID)
			if got.State != "completed" {
				t.Fatalf("persisted finalization wake was lost: %+v", got)
			}
		})
	}
}
