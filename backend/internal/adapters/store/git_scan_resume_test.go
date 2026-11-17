package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// checkGitScanProviderEvidenceResume is shared by the FS and PostgreSQL
// stores: new provider evidence reopens scans stopped for an unavailable
// commit or exhausted retries, and nothing else reopens anything.
func checkGitScanProviderEvidenceResume(t *testing.T, st scanTestStore) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	origin := "https://github.com/example/resume"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	tick := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	sha := func(i int) string { return fmt.Sprintf("%040x", i+0xc0ffee) }
	get := func(commit string) domain.GitScanJob {
		t.Helper()
		job, err := st.GetGitScan(ctx, repo, domain.NewGitScan(repo, origin, commit, clock).ID)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	// stop claims the only claimable scan and records the failure outcome.
	stop := func(commit, state, reason string, next time.Duration) domain.GitScanJob {
		t.Helper()
		claimed, err := st.ClaimGitScan(ctx, repo, tick(), time.Minute)
		if err != nil || claimed.Commit != commit {
			t.Fatal("unexpected claim", claimed, err)
		}
		failed := claimed
		failed.State, failed.Reason = state, reason
		failed.UpdatedAt = tick()
		failed.LeaseUntil = time.Time{}
		failed.NextAttempt = failed.UpdatedAt.Add(next)
		if err = st.FinishGitScan(ctx, domain.GitScanFinish{Job: failed}); err != nil {
			t.Fatal(err)
		}
		return failed
	}
	queue := func(commit string, created time.Time) {
		t.Helper()
		if err := st.EnqueueGitScan(ctx, domain.NewGitScan(repo, origin, commit, created)); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(ref, before, after string) {
		t.Helper()
		o := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: ref, Before: before, After: after}.WithID()
		if err := st.RecordGitRefObservation(ctx, o, tick()); err != nil {
			t.Fatal(err)
		}
	}
	unavailable, integrity, exhausted, pending, child := sha(1), sha(2), sha(3), sha(4), sha(5)
	old := clock.Add(-59 * 24 * time.Hour)
	queue(unavailable, old)
	stop(unavailable, "attention", domain.GitScanCommitUnavailable, 512*time.Second)
	queue(integrity, old)
	stop(integrity, "attention", "integrity_check_failed", 512*time.Second)
	queue(exhausted, old)
	stop(exhausted, "attention", domain.GitScanRetryLimit, 512*time.Second)
	queue(pending, clock)
	waiting := stop(pending, "retrying", domain.GitScanCommitPending, 24*time.Hour)

	// Client history (EnqueueGitScan) is not provider evidence.
	queue(unavailable, tick())
	if got := get(unavailable); got.State != "attention" || got.Reason != domain.GitScanCommitUnavailable {
		t.Fatal("client enqueue reopened a stopped scan", got)
	}

	// A new observation naming the commit reopens it immediately.
	stopped := get(unavailable)
	observe("refs/heads/main", "", unavailable)
	if got := get(unavailable); got.State != "waiting" || got.Reason != "" || got.Version != stopped.Version+1 || got.Attempts != stopped.Attempts || !got.NextAttempt.Equal(clock) || !got.LeaseUntil.IsZero() {
		t.Fatal("observation did not reopen an unavailable commit", got)
	}
	// Replaying the same observation (reconciliation re-reads heads) does not.
	stop(unavailable, "attention", domain.GitScanCommitUnavailable, 512*time.Second)
	observe("refs/heads/main", "", unavailable)
	if got := get(unavailable); got.State != "attention" {
		t.Fatal("replayed observation reopened a scan", got)
	}

	// Evidence failures stay stopped; exhausted retries reopen.
	observe("refs/heads/other", integrity, exhausted)
	if got := get(integrity); got.State != "attention" || got.Reason != "integrity_check_failed" {
		t.Fatal("observation reopened an integrity failure", got)
	}
	if got := get(exhausted); got.State != "waiting" || got.Reason != "" {
		t.Fatal("observation did not reopen exhausted retries", got)
	}
	stop(exhausted, "attention", domain.GitScanRetryLimit, 512*time.Second)

	// A scan that is still retrying keeps its schedule.
	observe("refs/heads/pending", "", pending)
	if got := get(pending); got.State != "retrying" || got.Version != waiting.Version || !got.NextAttempt.Equal(waiting.NextAttempt) {
		t.Fatal("observation rewrote a retrying scan", got)
	}

	// A parent named by an indexed provider commit is provider evidence too.
	queue(child, tick())
	claimed, err := st.ClaimGitScan(ctx, repo, tick(), time.Minute)
	if err != nil || claimed.Commit != child {
		t.Fatal("unexpected claim", claimed, err)
	}
	indexed := claimed
	indexed.State, indexed.Indexed = "waiting", true
	indexed.UpdatedAt, indexed.LeaseUntil = tick(), time.Time{}
	indexed.NextAttempt = indexed.UpdatedAt
	delta := domain.GitCommitDelta{Commit: child, Parents: []string{unavailable}, Parent: unavailable, Complete: true,
		Changes: []domain.GitPathChange{{Path: "file", After: domain.GitEntry{OID: sha(100), Mode: "100644"}}}}
	if err = st.FinishGitScan(ctx, domain.GitScanFinish{Job: indexed, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(repo, origin, delta)}, Parents: []domain.GitScanJob{domain.NewGitScan(repo, origin, unavailable, indexed.UpdatedAt)}}); err != nil {
		t.Fatal(err)
	}
	if got := get(unavailable); got.State != "waiting" || got.Reason != "" || !got.NextAttempt.Equal(indexed.UpdatedAt) {
		t.Fatal("indexed descendant did not reopen its unavailable parent", got)
	}

	// Manual retry still reopens any stopped scan.
	if err = st.RetryGitScan(ctx, repo, get(integrity).ID, tick()); err != nil {
		t.Fatal(err)
	}
	if got := get(integrity); got.State != "waiting" || got.Reason != "" {
		t.Fatal("manual retry no longer reopens a scan", got)
	}
}

func TestFSGitScanProviderEvidenceResume(t *testing.T) {
	checkGitScanProviderEvidenceResume(t, NewFSStore(t.TempDir()))
}
