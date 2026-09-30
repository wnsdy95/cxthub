//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func checkPGPRJobDiagnosticRollback(t *testing.T, st *PostgresStore) {
	t.Helper()
	ctx := context.Background()
	j := diagnosticStoreJob(t, st)
	if _, err := st.EnqueuePRJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback PR finish")
	outcome := claim
	outcome.State, outcome.UpdatedAt, outcome.LeaseUntil = "completed", claim.UpdatedAt.Add(time.Second), time.Time{}
	err = st.WithinRepository(ctx, j.RepoID, func(txctx context.Context) error {
		if err := st.FinishPRJob(txctx, outcome); err != nil {
			return err
		}
		inside, err := st.GetPRJob(txctx, j.RepoID, j.ID)
		if err != nil {
			return err
		}
		if inside.State != "completed" || len(inside.Diagnostics.Events) != 3 || inside.Diagnostics.Events[2].Kind != "finished" {
			t.Fatal("nested finish not visible inside repository transaction")
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("outer rollback: %v", err)
	}
	requireDiagnosticJobUnchanged(t, st, claim)
	if err := st.WithinRepository(ctx, j.RepoID, func(txctx context.Context) error {
		return st.FinishPRJob(txctx, outcome)
	}); err != nil {
		t.Fatal(err)
	}
	committed := readDiagnosticJob(t, st, j)
	if committed.State != "completed" || len(committed.Diagnostics.Events) != 3 {
		t.Fatal("committed nested finish missing")
	}
}

func checkPGPRJobDiagnosticLegacy(t *testing.T, st *PostgresStore) {
	t.Helper()
	ctx := context.Background()
	j := diagnosticStoreJob(t, st)
	j.Attempts, j.Version, j.Reason = 17, 31, "temporary_failure"
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	// Seed only this fixture's row as a pre-diagnostics payload.
	if _, err := st.db(ctx).Exec(ctx, `INSERT INTO pr_promotion_jobs(repo_id,id,payload,state,created_at,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		j.RepoID, j.ID, raw, j.State, j.CreatedAt, j.NextAttempt, j.LeaseUntil, j.Version); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readDiagnosticJob(t, st, j), j) {
		t.Fatal("legacy read synthesized diagnostics")
	}
	claim, err := st.ClaimPRJob(ctx, j.RepoID, j.ID, j.CreatedAt.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Attempts != 18 || claim.Diagnostics.TotalClaims != 1 || !claim.Diagnostics.Since.Equal(claim.UpdatedAt) || len(claim.Diagnostics.Events) != 1 || claim.Diagnostics.Events[0].Reason != "" {
		t.Fatal("legacy PG claim inferred events or lifetime claims")
	}
}
