package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func diagnosticStoreJob(t *testing.T, st sourceJobStore) domain.PRPromotionJob {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := st.PutRepo(context.Background(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	j := sourceJob(repo, 352, time.Unix(1000, 0).UTC())
	j.State, j.Reason, j.Attempts = "waiting", "", 0
	j.GitOrigin, j.BaseBranchID = "https://example.test/diagnostics", "main"
	return j
}

func readDiagnosticJob(t *testing.T, st sourceJobStore, j domain.PRPromotionJob) domain.PRPromotionJob {
	t.Helper()
	got, err := st.GetPRJob(context.Background(), j.RepoID, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("persisted diagnostic validation: %v", err)
	}
	return got
}

func requireDiagnosticJobUnchanged(t *testing.T, st sourceJobStore, before domain.PRPromotionJob) {
	t.Helper()
	if !reflect.DeepEqual(before, readDiagnosticJob(t, st, before)) {
		t.Fatal("ineffective operation changed job or diagnostics")
	}
}

func checkPRJobDiagnostics(t *testing.T, st sourceJobStore) {
	t.Helper()
	ctx := context.Background()
	t.Run("failure retry success replay and repository isolation", func(t *testing.T) {
		j := diagnosticStoreJob(t, st)
		// Enqueue does not trust even invalid replacement trails from a caller.
		j.Diagnostics = &domain.PRJobDiagnostics{TotalClaims: 999, Events: []domain.PRJobDiagnostic{{Kind: "raw-secret-error"}}}
		queued, err := st.EnqueuePRJob(ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		d := queued.Diagnostics
		if d == nil || !d.Since.Equal(j.CreatedAt) || d.TotalClaims != 0 || d.Dropped != 0 || len(d.Events) != 1 || d.Events[0].Kind != "queued" {
			t.Fatal("enqueue did not start an owned, bounded trail")
		}
		claim, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		outcome := claim
		outcome.State, outcome.Reason, outcome.FailureClass = "retrying", "temporary_failure", "deadline_exceeded"
		outcome.UpdatedAt, outcome.NextAttempt, outcome.LeaseUntil = j.CreatedAt.Add(1500*time.Millisecond), j.CreatedAt.Add(time.Minute), time.Time{}
		outcome.Diagnostics = j.Diagnostics
		outcome.Attempts, outcome.CreatedAt, outcome.BaseBranchID = 999, j.CreatedAt.Add(time.Hour), "caller replacement"
		if err := st.FinishPRJob(ctx, outcome); err != nil {
			t.Fatal(err)
		}
		failed := readDiagnosticJob(t, st, j)
		if failed.Attempts != 1 || !failed.CreatedAt.Equal(j.CreatedAt) || failed.BaseBranchID != j.BaseBranchID || failed.FailureClass != "deadline_exceeded" || failed.Diagnostics.TotalClaims != 1 || len(failed.Diagnostics.Events) != 3 {
			t.Fatal("finish did not preserve authoritative claim and diagnostics")
		}
		failure := failed.Diagnostics.Events[2]
		if failure.Kind != "finished" || failure.Reason != "temporary_failure" || failure.FailureClass != "deadline_exceeded" || failure.ElapsedMS == nil || *failure.ElapsedMS != 1500 || failure.Attempt != 1 {
			t.Fatal("failed claim diagnostic lost safe reason, class, attempt or elapsed")
		}
		if err := st.RetryPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		retried := readDiagnosticJob(t, st, j)
		if retried.Attempts != 0 || retried.Reason != "" || retried.FailureClass != "" || retried.Version != claim.Version+1 || len(retried.Diagnostics.Events) != 4 || retried.Diagnostics.Events[3].Kind != "retry_requested" || !reflect.DeepEqual(failure, retried.Diagnostics.Events[2]) {
			t.Fatal("retry lost failed attempt history or retained current failure class")
		}
		if err := st.RetryPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		replay := j
		replay.Version, replay.Attempts, replay.State, replay.Reason = 200, 80, "attention", "invalid_request"
		got, err := st.EnqueuePRJob(ctx, replay)
		if err != nil || !reflect.DeepEqual(got, retried) {
			t.Fatalf("enqueue replay replaced delivery state: %v", err)
		}
		requireDiagnosticJobUnchanged(t, st, retried)
		claim, err = st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(4*time.Second), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claim.FailureClass != "" || claim.Attempts != 1 || claim.Diagnostics.TotalClaims != 2 {
			t.Fatal("new claim confused reset attempts and observed total claims")
		}
		outcome = claim
		outcome.State, outcome.Reason, outcome.FailureClass = "completed", "", "canceled"
		outcome.UpdatedAt, outcome.LeaseUntil = claim.UpdatedAt.Add(time.Second), time.Time{}
		if err := st.FinishPRJob(ctx, outcome); err != nil {
			t.Fatal(err)
		}
		completed := readDiagnosticJob(t, st, j)
		if completed.FailureClass != "" || len(completed.Diagnostics.Events) != 6 || completed.Diagnostics.Events[5].FailureClass != "" || !reflect.DeepEqual(failure, completed.Diagnostics.Events[2]) {
			t.Fatal("success failed to clear current class or retain earlier failure")
		}
		if err := st.RetryPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishPRJob(ctx, outcome); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("repeated finish was not fenced")
		}
		requireDiagnosticJobUnchanged(t, st, completed)
		other := domain.HashContent([]byte(t.Name() + " other repository"))
		if _, err := st.GetPRJob(ctx, other, j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("cross repository job read")
		}
		if _, err := st.ClaimPRJob(ctx, other, j.ID, j.CreatedAt.Add(time.Hour), time.Minute); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("cross repository claim")
		}
		if _, err := st.PutRepo(ctx, domain.Repo{ID: other, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		otherJob := j
		otherJob.RepoID, otherJob.ID = other, domain.PRPromotionID(other, j.PR.Number)
		otherQueued, err := st.EnqueuePRJob(ctx, otherJob)
		if err != nil || otherQueued.ID == j.ID || otherQueued.Diagnostics.TotalClaims != 0 || len(otherQueued.Diagnostics.Events) != 1 {
			t.Fatalf("cross repository enqueue diagnostics: %v", err)
		}
		requireDiagnosticJobUnchanged(t, st, completed)
	})
	t.Run("crash reclaim stale worker and invalid outcomes", func(t *testing.T) {
		j := diagnosticStoreJob(t, st)
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		claim, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(time.Second), time.Minute); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("active lease reclaimed")
		}
		if err := st.RetryPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("active claim manually retried")
		}
		recovered, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(2*time.Minute), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		d := recovered.Diagnostics
		if recovered.Version != claim.Version+1 || recovered.Attempts != 2 || d.TotalClaims != 2 || len(d.Events) != 3 || d.Events[2].Kind != "claimed" || d.Events[2].Reason != "lease_expired" {
			t.Fatal("crash recovery did not record a fenced reclaim")
		}
		claim.State, claim.Reason, claim.FailureClass = "attention", "temporary_failure", "canceled"
		claim.Diagnostics = &domain.PRJobDiagnostics{TotalClaims: 999}
		if err := st.FinishPRJob(ctx, claim); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("stale worker replaced current diagnostics")
		}
		for _, tc := range []struct {
			edit func(*domain.PRPromotionJob)
			want error
		}{
			{func(j *domain.PRPromotionJob) { j.PR.HeadBranch = "changed" }, domain.ErrConflict},
			{func(j *domain.PRPromotionJob) { j.GitOrigin = "changed" }, domain.ErrConflict},
			{func(j *domain.PRPromotionJob) { j.FailureClass = "raw-secret-error" }, domain.ErrValidation},
			{func(j *domain.PRPromotionJob) { j.Reason = "raw-secret-error" }, domain.ErrValidation},
			{func(j *domain.PRPromotionJob) { j.State = "raw-secret-error" }, domain.ErrValidation},
			{func(j *domain.PRPromotionJob) { j.State = "running" }, domain.ErrValidation},
		} {
			outcome := recovered
			outcome.State, outcome.Reason = "completed", ""
			tc.edit(&outcome)
			if err := st.FinishPRJob(ctx, outcome); !errors.Is(err, tc.want) {
				t.Fatal("invalid or conflicting worker outcome accepted")
			}
		}
		requireDiagnosticJobUnchanged(t, st, recovered)
		// Existing callers/tests can finish with a clock older than the claim.
		outcome := recovered
		outcome.State, outcome.Reason, outcome.FailureClass = "retrying", "temporary_failure", "unspecified"
		outcome.UpdatedAt, outcome.NextAttempt, outcome.LeaseUntil = j.CreatedAt.Add(time.Minute), j.CreatedAt.Add(3*time.Minute), time.Time{}
		if err := st.FinishPRJob(ctx, outcome); err != nil {
			t.Fatal(err)
		}
		finished := readDiagnosticJob(t, st, j)
		e := finished.Diagnostics.Events[3]
		if e.ElapsedMS == nil || *e.ElapsedMS != 0 || e.Attempt != 2 || e.Version != recovered.Version || e.FailureClass != "unspecified" {
			t.Fatal("finish used caller clock/attempt history instead of current fenced claim")
		}
	})
	t.Run("bounded durable counters survive attempts reset", func(t *testing.T) {
		j := diagnosticStoreJob(t, st)
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 40; i++ {
			now := j.CreatedAt.Add(time.Duration(i) * time.Second)
			claim, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			claim.State, claim.Reason, claim.FailureClass = "attention", "integrity_check_failed", "unspecified"
			claim.UpdatedAt, claim.LeaseUntil = now.Add(time.Millisecond), time.Time{}
			if err := st.FinishPRJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			if err := st.RetryPRJob(ctx, j.RepoID, j.ID, now.Add(2*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		got := readDiagnosticJob(t, st, j)
		d := got.Diagnostics
		if got.Attempts != 0 || len(d.Events) != 32 || d.TotalClaims != 40 || d.Dropped != 89 || !d.Since.Equal(j.CreatedAt) || d.Events[31].Kind != "retry_requested" {
			t.Fatal("durable bounded diagnostics lost observation counts or latest transitions")
		}
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		requireDiagnosticJobUnchanged(t, st, got)
	})
	t.Run("source wake retains failure and repeats unchanged", func(t *testing.T) {
		repo, proof, now := sourceJobFixture(t, st)
		j := sourceJob(repo, 352, now)
		j.State, j.Reason, j.Attempts = "waiting", "", 0
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		claim, err := st.ClaimPRJob(ctx, repo, j.ID, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		claim.State, claim.Reason, claim.FailureClass = "attention", "source_finalization_required", "unspecified"
		claim.UpdatedAt, claim.LeaseUntil = now.Add(time.Second), time.Time{}
		if err := st.FinishPRJob(ctx, claim); err != nil {
			t.Fatal(err)
		}
		before := readDiagnosticJob(t, st, j)
		if err := st.WakePRSourceJobs(ctx, repo, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		requireDiagnosticJobUnchanged(t, st, before)
		storeSourcePublication(t, st, proof)
		if err := st.WakePRSourceJobs(ctx, domain.HashContent([]byte("other repository")), now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		requireDiagnosticJobUnchanged(t, st, before)
		if err := st.WakePRSourceJobs(ctx, repo, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		woken := readDiagnosticJob(t, st, j)
		if woken.State != "waiting" || woken.Attempts != 0 || woken.FailureClass != "" || woken.Reason != "" || woken.Version != before.Version+1 || woken.Diagnostics.TotalClaims != 1 || len(woken.Diagnostics.Events) != 4 || woken.Diagnostics.Events[3].Kind != "source_available" || !reflect.DeepEqual(woken.Diagnostics.Events[:3], before.Diagnostics.Events) {
			t.Fatal("source wake replaced failure history")
		}
		for i := 0; i < 2; i++ {
			if err := st.WakePRSourceJobs(ctx, "", now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.RetryPRJob(ctx, repo, j.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		requireDiagnosticJobUnchanged(t, st, woken)
	})
}

func TestFSPRJobDiagnostics(t *testing.T) {
	checkPRJobDiagnostics(t, NewFSStore(t.TempDir()))
}

func TestFSPRJobDiagnosticsLegacyReadAndFirstTransitions(t *testing.T) {
	for _, transition := range []string{"claim", "retry", "wake"} {
		t.Run(transition, func(t *testing.T) {
			st := NewFSStore(t.TempDir())
			ctx := context.Background()
			repo, proof, now := sourceJobFixture(t, st)
			j := sourceJob(repo, 352, now)
			j.Attempts, j.Version = 17, 31
			if transition == "claim" {
				j.State, j.Reason = "retrying", "temporary_failure"
			}
			if err := st.writePRJob(j); err != nil {
				t.Fatal(err)
			}
			legacy := readDiagnosticJob(t, st, j)
			if legacy.Diagnostics != nil || !reflect.DeepEqual(legacy, j) {
				t.Fatal("legacy read synthesized diagnostics")
			}
			replayed, err := st.EnqueuePRJob(ctx, j)
			if err != nil || !reflect.DeepEqual(replayed, j) {
				t.Fatalf("legacy replay synthesized history: %v", err)
			}
			now = now.Add(time.Hour)
			wantKind, wantClaims, wantAttempt := "claimed", uint64(1), 18
			switch transition {
			case "claim":
				if _, err := st.ClaimPRJob(ctx, repo, j.ID, now, time.Minute); err != nil {
					t.Fatal(err)
				}
			case "retry":
				wantKind, wantClaims, wantAttempt = "retry_requested", 0, 0
				if err := st.RetryPRJob(ctx, repo, j.ID, now); err != nil {
					t.Fatal(err)
				}
			case "wake":
				wantKind, wantClaims, wantAttempt = "source_available", 0, 0
				storeSourcePublication(t, st, proof)
				if err := st.WakePRSourceJobs(ctx, repo, now); err != nil {
					t.Fatal(err)
				}
			}
			got := readDiagnosticJob(t, st, j)
			d := got.Diagnostics
			if d == nil || !d.Since.Equal(now) || d.TotalClaims != wantClaims || d.Dropped != 0 || len(d.Events) != 1 || d.Events[0].Kind != wantKind || d.Events[0].Attempt != wantAttempt || d.Events[0].Reason != "" {
				t.Fatal("first observed transition inferred legacy history")
			}
		})
	}
}
