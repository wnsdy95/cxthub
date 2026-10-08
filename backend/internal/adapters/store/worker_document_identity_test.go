package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type workerQueueStore interface {
	outbound.MetadataStore
	outbound.RepositoryDocumentIdentityStore
	outbound.PRJobStore
	outbound.GitChangeStore
	outbound.GitScanStore
	outbound.GitHeadScanStore
}

func checkWorkerQueueIdentity(t *testing.T, st workerQueueStore) {
	t.Helper()
	for _, when := range []string{"before-claim", "after-claim"} {
		t.Run(when, func(t *testing.T) {
			ctx := context.Background()
			repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			origin := "https://github.com/example/synthetic-workers"
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			pr := domain.PullRequestMerge{Number: 17, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			p := domain.PRPromotionJob{ID: domain.PRPromotionID(repo, pr.Number), RepoID: repo, GitOrigin: origin, PR: pr, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
			var enqueueErr error
			p, enqueueErr = st.EnqueuePRJob(ctx, p)
			if enqueueErr != nil {
				t.Fatal(enqueueErr)
			}
			c := domain.GitChangeJob{RepoID: repo, GitOrigin: origin, Request: domain.GitChangeRequest{Target: pr.HeadSHA, Commit: pr.MergeSHA}, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
			c.ID = domain.GitChangeID(repo, origin, c.Request)
			if _, err := st.EnqueueGitChange(ctx, c); err != nil {
				t.Fatal(err)
			}
			s := domain.NewGitScan(repo, origin, pr.HeadSHA, now)
			if err := st.EnqueueGitScan(ctx, s); err != nil {
				t.Fatal(err)
			}
			var head domain.GitHeadScan
			if when == "after-claim" {
				var err error
				if p, err = st.ClaimPRJob(ctx, repo, p.ID, now, time.Minute); err != nil {
					t.Fatal(err)
				}
				if c, err = st.ClaimGitChange(ctx, repo, c.ID, now, time.Minute); err != nil {
					t.Fatal(err)
				}
				if s, err = st.ClaimGitScan(ctx, repo, now, time.Minute); err != nil {
					t.Fatal(err)
				}
				if head, err = st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
			reject := func(err error) {
				t.Helper()
				if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
					t.Fatalf("expected policy rejection: %v", err)
				}
			}
			if when == "before-claim" {
				_, e := st.ClaimPRJob(ctx, repo, p.ID, now, time.Minute)
				reject(e)
				_, e = st.ClaimGitChange(ctx, repo, c.ID, now, time.Minute)
				reject(e)
				_, e = st.ClaimGitScan(ctx, repo, now, time.Minute)
				reject(e)
				_, e = st.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
				reject(e)
			} else {
				pp := p
				pp.State = "attention"
				pp.Reason = "synthetic"
				cc := c
				cc.State = "attention"
				ss := s
				ss.State = "attention"
				reject(st.FinishPRJob(ctx, pp))
				reject(st.FinishGitChange(ctx, cc))
				reject(st.FinishGitScan(ctx, domain.GitScanFinish{Job: ss}))
				reject(st.FinishGitHeadScan(ctx, head, nil, false, now))
				reject(st.FailGitHeadScan(ctx, head, now))
				got, e := st.GetGitHeadScan(ctx, repo, origin)
				if e != nil || !reflect.DeepEqual(got, head) {
					t.Fatal("head changed", got, e)
				}
			}
			pp, e := st.GetPRJob(ctx, repo, p.ID)
			if e != nil || !reflect.DeepEqual(pp, p) {
				t.Fatal("PR changed", pp, e)
			}
			cc, e := st.GetGitChange(ctx, repo, c.ID)
			if e != nil || !reflect.DeepEqual(cc, c) {
				t.Fatal("change changed", cc, e)
			}
			ss, e := st.GetGitScan(ctx, repo, s.ID)
			if e != nil || !reflect.DeepEqual(ss, s) {
				t.Fatal("scan changed", ss, e)
			}
		})
	}
}
func TestWorkerFSQueueIdentity(t *testing.T) { checkWorkerQueueIdentity(t, NewFSStore(t.TempDir())) }

func TestWorkerFSGlobalClaimSkipsUnsupported(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	now := time.Now().UTC()
	var legacy domain.ContentHash
	for _, root := range []bool{true, false} {
		repo := domain.HashContent([]byte(time.Now().String()))
		origin := "https://github.com/example/worker"
		if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
			t.Fatal(err)
		}
		j := domain.NewGitScan(repo, origin, strings.Repeat("a", 40), now)
		if err := st.EnqueueGitScan(ctx, j); err != nil {
			t.Fatal(err)
		}
		if root {
			if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
		} else {
			legacy = repo
		}
		now = now.Add(time.Second)
	}
	got, err := st.ClaimGitScan(ctx, "", now, time.Minute)
	if err != nil || got.RepoID != legacy {
		t.Fatal("unsupported job starved legacy work", got, err)
	}
}

func TestWorkerFSMetadataPin(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo}); e != nil {
		t.Fatal(e)
	}
	release, e := st.pinWorkerRepoPolicy(ctx, repo)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
	select {
	case e := <-done:
		release()
		t.Fatal("opt-in crossed retained policy", e)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if release, e = st.pinWorkerRepoPolicy(ctx, repo); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		if release != nil {
			release()
		}
		t.Fatal(e)
	}
}

func checkWorkerPRWakePolicy(t *testing.T, st interface {
	sourceJobStore
	outbound.RepositoryDocumentIdentityStore
}) {
	ctx := context.Background()
	repo, proof, now := sourceJobFixture(t, st)
	j, e := st.EnqueuePRJob(ctx, sourceJob(repo, 1, now))
	if e != nil {
		t.Fatal(e)
	}
	storeSourcePublication(t, st, proof)
	if e = st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); e != nil {
		t.Fatal(e)
	}
	if e = st.WakePRSourceJobs(ctx, "", now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e = st.WakePRSourceJobs(ctx, repo, now.Add(time.Minute)); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal(e)
	}
	got, e := st.GetPRJob(ctx, repo, j.ID)
	if e != nil || !reflect.DeepEqual(got, j) {
		t.Fatal("wake changed unsupported queue", got, e)
	}
}
func TestWorkerFSPRWakePolicy(t *testing.T) { checkWorkerPRWakePolicy(t, NewFSStore(t.TempDir())) }
