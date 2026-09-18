//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"testing"
	"time"
)

func TestPGGitChanges(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	checkGitChanges(t, st)
	// Finish + revision roll back together after a failure between both writes.
	j, err := st.ClaimGitChange(ctx, "", "", time.Now().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.RepositoryRevision(ctx, j.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("injected after result publication")
	err = st.WithinRepository(ctx, j.RepoID, func(tx context.Context) error {
		done := j
		done.State = "completed"
		done.Result = &domain.GitReversalEvidence{Target: j.Request.Target, Commit: j.Request.Commit, Coverage: "unverified", Reason: "no_exact_inverse"}
		if err := st.FinishGitChange(tx, done); err != nil {
			return err
		}
		if err := st.AdvanceRepositoryRevision(tx, j.RepoID, false); err != nil {
			return err
		}
		return fail
	})
	if !errors.Is(err, fail) {
		t.Fatal(err)
	}
	other, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.GetGitChange(ctx, j.RepoID, j.ID)
	if err != nil || got.State != "running" || got.Result != nil {
		t.Fatalf("rollback leaked result %+v %v", got, err)
	}
	after, err := other.RepositoryRevision(ctx, j.RepoID)
	if err != nil || after != before {
		t.Fatalf("rollback leaked revision %+v %v", after, err)
	}
	recovered, err := other.ClaimGitChange(ctx, j.RepoID, j.ID, time.Now().Add(2*time.Hour), time.Minute)
	if err != nil || recovered.Version <= j.Version {
		t.Fatalf("restart lease %+v %v", recovered, err)
	}
}
