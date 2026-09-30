package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type prDiagnosticFailureStore struct {
	*store.FSStore
	failure error
}

func TestPRJobDiagnosticCanceledRequestPersistsRetry(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			svc, st, job, proof := finalizationJobFixture(t)
			ctx := systemTestContext()
			publishPRSource(t, svc, proof)
			before, err := st.GetRef(ctx, job.RepoID, domain.RefBranch, "main")
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := st.ClaimPRJob(ctx, job.RepoID, job.ID, time.Now().UTC(), prJobLease)
			if err != nil {
				t.Fatal(err)
			}
			var request context.Context
			var cancel context.CancelFunc
			class, want := "canceled", context.Canceled
			if deadline {
				request, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				class, want = "deadline_exceeded", context.DeadlineExceeded
			} else {
				request, cancel = context.WithCancel(ctx)
			}
			cancel()
			if _, err = svc.runPRJob(request, claimed); !errors.Is(err, want) {
				t.Fatalf("request result: %v", err)
			}
			got, err := st.GetPRJob(ctx, job.RepoID, job.ID)
			if err != nil || got.State != "retrying" || got.FailureClass != class || !got.LeaseUntil.IsZero() {
				t.Fatalf("retry was not durably released: state=%s class=%s err=%v", got.State, got.FailureClass, err)
			}
			last := got.Diagnostics.Events[len(got.Diagnostics.Events)-1]
			if last.Kind != "finished" || last.FailureClass != class {
				t.Fatal("canceled request lost its diagnostic outcome")
			}
			after, err := st.GetRef(ctx, job.RepoID, domain.RefBranch, "main")
			if err != nil || before.Target != after.Target {
				t.Fatal("already canceled work moved the base context")
			}
			rows, err := svc.ListHistory(ctx, job.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range rows {
				if e.PRCompleted {
					t.Fatal("already canceled work recorded a completed PR")
				}
			}
		})
	}
}

func (s *prDiagnosticFailureStore) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, s.failure
}

func TestPRJobDiagnosticFailureSurvivesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{
		{"deadline", errors.Join(errors.New("synthetic-private-detail"), context.DeadlineExceeded), "deadline_exceeded"},
		{"cancellation", context.Canceled, "canceled"},
		{"other", errors.New("synthetic-private-detail"), "unspecified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, job, proof := finalizationJobFixture(t)
			ctx := systemTestContext()
			claimed, err := st.ClaimPRJob(ctx, job.RepoID, job.ID, time.Now().UTC(), prJobLease)
			if err != nil {
				t.Fatal(err)
			}
			svc.meta = &prDiagnosticFailureStore{FSStore: st, failure: tc.err}
			if _, err := svc.runPRJob(ctx, claimed); !errors.Is(err, tc.err) {
				t.Fatalf("failure was lost: %v", err)
			}
			failed, err := st.GetPRJob(ctx, job.RepoID, job.ID)
			if err != nil || failed.State != "retrying" || failed.FailureClass != tc.class {
				t.Fatalf("failure classification: state=%s class=%s err=%v", failed.State, failed.FailureClass, err)
			}
			svc.meta = st
			publishPRSource(t, svc, proof)
			claimed, err = st.ClaimPRJob(ctx, job.RepoID, job.ID, time.Now().Add(time.Hour), prJobLease)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.runPRJob(ctx, claimed); err != nil {
				t.Fatal(err)
			}
			completed, err := st.GetPRJob(ctx, job.RepoID, job.ID)
			if err != nil || completed.State != "completed" || completed.Reason != "" || completed.FailureClass != "" {
				t.Fatalf("completion: state=%s reason=%s class=%s err=%v", completed.State, completed.Reason, completed.FailureClass, err)
			}
			failures := 0
			for _, e := range completed.Diagnostics.Events {
				if e.Kind == "finished" && e.State == "retrying" && e.Reason == "temporary_failure" && e.FailureClass == tc.class {
					failures++
				}
			}
			if failures != 1 || completed.Diagnostics.TotalClaims != 2 {
				t.Fatalf("prior failure lost or duplicated: failures=%d claims=%d", failures, completed.Diagnostics.TotalClaims)
			}
			body, err := json.Marshal(completed)
			if err != nil || strings.Contains(string(body), "synthetic-private-detail") {
				t.Fatal("raw upstream error entered the diagnostic contract")
			}
			// Completed request replay must not move the ref or append a fake attempt.
			if _, err = svc.DeliverPRPromotion(ctx, job.RepoID, job.PR); err != nil {
				t.Fatal(err)
			}
			replayed, err := st.GetPRJob(ctx, job.RepoID, job.ID)
			after, _ := json.Marshal(replayed)
			if err != nil || string(body) != string(after) {
				t.Fatal("completed replay changed delivery diagnostics")
			}
		})
	}
}
