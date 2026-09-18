package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"sync"
	"testing"
	"time"
)

type scanTestStore interface {
	outbound.GitScanStore
	outbound.GitChangeStore
	outbound.MetadataStore
}

func checkGitScans(t *testing.T, st scanTestStore) domain.GitScanJob {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	origin := "https://github.com/example/project"
	now := time.Now().UTC()
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	sha := func(i int) string { return fmt.Sprintf("%040x", i) }
	j := domain.NewGitScan(repo, origin, sha(1), now)
	if err := st.EnqueueGitScan(ctx, j); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan domain.GitScanJob, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := st.ClaimGitScan(ctx, repo, now, time.Minute)
			if err == nil {
				claims <- got
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	count := 0
	var first domain.GitScanJob
	for got := range claims {
		first = got
		count++
	}
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
	if err := st.EnqueueGitScan(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryGitScan(ctx, repo, j.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("retry stole live lease", err)
	}
	second, err := st.ClaimGitScan(ctx, repo, now.Add(2*time.Minute), time.Minute)
	if err != nil || second.Version <= first.Version {
		t.Fatal(second, err)
	}
	next := second
	next.State = "waiting"
	next.Indexed = true
	next.LeaseUntil = time.Time{}
	before := domain.GitEntry{OID: sha(1000), Mode: "100644"}
	after := domain.GitEntry{OID: sha(1001), Mode: "100755"}
	d := domain.GitCommitDelta{Commit: j.Commit, Parents: []string{sha(2)}, Parent: sha(2), Complete: true, Changes: []domain.GitPathChange{{Path: "file", Before: before, After: after}}}
	parent := domain.NewGitScan(repo, origin, sha(2), now)
	finish := domain.GitScanFinish{Job: next, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(repo, origin, d)}, Parents: []domain.GitScanJob{parent}}
	// A stale worker must not publish even the index or its child work.
	stale := finish
	stale.Job = first
	stale.Job.State = "waiting"
	stale.Job.Indexed = true
	if err = st.FinishGitScan(ctx, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale finish", err)
	}
	if _, err = st.GetGitDelta(ctx, repo, origin, j.Commit, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("stale index leaked", err)
	}
	if err = st.FinishGitScan(ctx, finish); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetGitScan(ctx, repo, parent.ID); err != nil {
		t.Fatal("ancestor work lost", err)
	}
	if err = st.FinishGitScan(ctx, finish); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("duplicate finish overwrote", err)
	}
	if _, err = st.GetGitDelta(ctx, domain.HashContent([]byte("other")), origin, j.Commit, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross repo read", err)
	}
	if _, err = st.GetGitDelta(ctx, repo, "https://github.com/other/repo", j.Commit, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross origin read", err)
	}
	// Index later commits without depending on wall-clock ordering. Explicit
	// comparison-parent identity and cursor pagination must preserve all matches.
	for i := 2; i <= 5; i++ {
		x := domain.NewGitScan(repo, origin, sha(i), now)
		if err = st.EnqueueGitScan(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	indexed := map[string]bool{j.Commit: true}
	for len(indexed) < 5 {
		x, e := st.ClaimGitScan(ctx, repo, now.Add(10*time.Minute), time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		p := domain.GitScanFinish{Job: x}
		p.Job.State = "completed"
		p.Job.Indexed = true
		if !x.Indexed {
			delta := domain.GitCommitDelta{Commit: x.Commit, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{{Path: "file", Before: after, After: before}}}
			p.Deltas = []domain.GitDeltaRecord{domain.NewGitDelta(repo, origin, delta)}
			indexed[x.Commit] = true
		}
		if e = st.FinishGitScan(ctx, p); e != nil {
			t.Fatal(e)
		}
	}
	curs := ""
	seen := map[string]bool{}
	for {
		page, e := st.FindGitInverses(ctx, repo, origin, j.Commit, curs, 2)
		if e != nil {
			t.Fatal(e)
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			if seen[c.Cursor] || c.Cursor <= curs {
				t.Fatal("cursor repeated")
			}
			seen[c.Cursor] = true
			curs = c.Cursor
		}
	}
	if len(seen) != 4 {
		t.Fatalf("inverse matches %d", len(seen))
	}
	if got, e := st.FindGitInverses(ctx, repo, origin+"-other", j.Commit, "", 50); e != nil || len(got) != 0 {
		t.Fatal("origin lookup leak", e)
	}
	// Finish any discovery-only job left by the hash-ordered claim sequence.
	for {
		x, e := st.ClaimGitScan(ctx, repo, now.Add(10*time.Minute), time.Minute)
		if errors.Is(e, domain.ErrNotFound) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if !x.Indexed {
			t.Fatal("unindexed fixture", x)
		}
		x.State = "completed"
		if e = st.FinishGitScan(ctx, domain.GitScanFinish{Job: x}); e != nil {
			t.Fatal(e)
		}
	}
	extra := domain.NewGitScan(repo, origin, strings.Repeat("f", 40), now)
	if err = st.EnqueueGitScan(ctx, extra); err != nil {
		t.Fatal(err)
	}
	return extra
}
func TestFSGitScans(t *testing.T) { st := NewFSStore(t.TempDir()); checkGitScans(t, st) }

func checkGitHeadScans(t *testing.T, st interface {
	outbound.GitHeadScanStore
	outbound.GitScanStore
	outbound.MetadataStore
}) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + "heads" + time.Now().String()))
	origin := "https://github.com/example/heads"
	now := time.Now().UTC()
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	j, err := st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
	if err != nil || j.Page != 1 {
		t.Fatal(j, err)
	}
	if _, err = st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("parallel page reader stole lease", err)
	}
	observation := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat("b", 40)}.WithID()
	if err = st.FinishGitHeadScan(ctx, j, []domain.GitRefObservation{observation}, true, now); err != nil {
		t.Fatal(err)
	}
	j2, err := st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
	if err != nil || j2.Page != 2 {
		t.Fatal("page did not persist", j2, err)
	}
	if err = st.FinishGitHeadScan(ctx, j, nil, false, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old page reset new cursor", err)
	}
	j3, err := st.ClaimGitHeadScan(ctx, repo, origin, now.Add(2*time.Minute), time.Minute)
	if err != nil || j3.Page != 2 || j3.Version <= j2.Version {
		t.Fatal("interrupted page not recovered", j3, err)
	}
	if err = st.FinishGitHeadScan(ctx, j3, []domain.GitRefObservation{observation}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("completed pass busy polls", err)
	}
	j4, err := st.ClaimGitHeadScan(ctx, repo, origin, now.Add(6*time.Minute), time.Minute)
	if err != nil || j4.Page != 1 {
		t.Fatal("fresh pass missing", j4, err)
	}
	jobs, err := st.ListGitScans(ctx, repo, "", 100)
	if err != nil || len(jobs) != 1 {
		t.Fatal("head acceptance missing/doubled", jobs, err)
	}
}
func TestFSGitHeadScans(t *testing.T) { checkGitHeadScans(t, NewFSStore(t.TempDir())) }
