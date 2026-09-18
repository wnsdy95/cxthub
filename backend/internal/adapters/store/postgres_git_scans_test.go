//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPGGitScans(t *testing.T) {
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
	job := checkGitScans(t, st)
	claimed, err := st.ClaimGitScan(ctx, job.RepoID, time.Now().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != job.ID {
		t.Fatal("wrong leftover job")
	}
	before, _ := st.RepositoryRevision(ctx, job.RepoID)
	sentinel := errors.New("after atomic index publication")
	delta := domain.GitCommitDelta{Commit: job.Commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}
	next := claimed
	next.Indexed = true
	next.State = "completed"
	err = st.WithinRepository(ctx, job.RepoID, func(tx context.Context) error {
		if e := st.FinishGitScan(tx, domain.GitScanFinish{Job: next, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(job.RepoID, job.GitOrigin, delta)}}); e != nil {
			return e
		}
		if e := st.AdvanceRepositoryRevision(tx, job.RepoID, false); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	other, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.GetGitScan(ctx, job.RepoID, job.ID)
	if err != nil || got.Indexed || got.State != "running" {
		t.Fatal("rollback leaked cursor", got, err)
	}
	if _, err = other.GetGitDelta(ctx, job.RepoID, job.GitOrigin, job.Commit, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rollback leaked index", err)
	}
	after, _ := other.RepositoryRevision(ctx, job.RepoID)
	if before != after {
		t.Fatal("rollback leaked revision")
	}
	recovered, err := other.ClaimGitScan(ctx, job.RepoID, time.Now().Add(2*time.Hour), time.Minute)
	if err != nil {
		t.Fatal("restart did not recover lease", err)
	}
	recovered.State = "completed"
	recovered.Indexed = true
	if err = other.FinishGitScan(ctx, domain.GitScanFinish{Job: recovered, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(job.RepoID, job.GitOrigin, delta)}}); err != nil {
		t.Fatal(err)
	}
	checkGitHeadScans(t, st)
}

func TestPGGitScansDependentPublicationRollback(t *testing.T) {
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
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	origin := "https://github.com/example/atomic"
	now := time.Now().UTC()
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	job := domain.NewGitScan(repo, origin, strings.Repeat("a", 40), now)
	st.EnqueueGitScan(ctx, job)
	j, err := st.ClaimGitScan(ctx, repo, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.GitScanFinish{Job: j, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(repo, origin, domain.GitCommitDelta{Commit: j.Commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true})}}
	p.Job.State = "waiting"
	p.Job.Indexed = true
	if err = st.FinishGitScan(ctx, p); err != nil {
		t.Fatal(err)
	}
	j, err = st.ClaimGitScan(ctx, repo, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r := domain.GitChangeRequest{Target: strings.Repeat("b", 40), Commit: j.Commit}
	change := domain.GitChangeJob{ID: domain.GitChangeID(repo, origin, r), RepoID: repo, GitOrigin: origin, Request: r, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	p = domain.GitScanFinish{Job: j, Changes: []domain.GitChangeJob{change}}
	p.Job.State = "completed"
	p.Job.Cursor = strings.Repeat("c", 128)
	sentinel := errors.New("after candidates before commit")
	err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if e := st.FinishGitScan(tx, p); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err = st.GetGitChange(ctx, repo, change.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("candidate escaped rollback", err)
	}
	stored, _ := st.GetGitScan(ctx, repo, j.ID)
	if stored.Cursor != "" || stored.State != "running" {
		t.Fatal("cursor escaped rollback", stored)
	}
	if err = st.FinishGitScan(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetGitChange(ctx, repo, change.ID); err != nil {
		t.Fatal("candidate lost", err)
	}
	// Reconciliation has the same all-or-nothing page/observation/commit queue.
	head, err := st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	obs := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/new", After: strings.Repeat("d", 40)}.WithID()
	err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if e := st.FinishGitHeadScan(tx, head, []domain.GitRefObservation{obs}, true, now); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	got, err := st.GetGitHeadScan(ctx, repo, origin)
	if err != nil || got.Page != 1 || got.State != "running" {
		t.Fatal("head cursor escaped rollback", got, err)
	}
	if _, err = st.GetGitScan(ctx, repo, domain.NewGitScan(repo, origin, obs.After, now).ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("head scan queue escaped rollback", err)
	}
	var count int
	if err = st.db(ctx).QueryRow(ctx, `SELECT count(*) FROM git_ref_observations WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
		t.Fatal("observation escaped rollback", count, err)
	}
}
