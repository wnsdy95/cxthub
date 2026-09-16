package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func checkPRJobs(t *testing.T, st interface {
	outbound.PRJobStore
	outbound.MetadataStore
}) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	_, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pr := domain.PullRequestMerge{Number: 991, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	j := domain.PRPromotionJob{ID: domain.PRPromotionID(repo, pr.Number), RepoID: repo, PR: pr, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	if _, err := st.EnqueuePRJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	bad := j
	bad.PR.HeadSHA = strings.Repeat("c", 40)
	if _, err := st.EnqueuePRJob(ctx, bad); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity replay=%v", err)
	}
	second := j
	second.PR.Number++
	second.ID = domain.PRPromotionID(repo, second.PR.Number)
	second.CreatedAt = now.Add(time.Millisecond)
	if _, err := st.EnqueuePRJob(ctx, second); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan domain.PRPromotionJob, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := st.ClaimPRJob(ctx, repo, "", now, time.Minute)
			if err == nil {
				claims <- got
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("claim=%v", err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	var first domain.PRPromotionJob
	count := 0
	for got := range claims {
		first = got
		count++
	}
	if count != 1 || first.ID != j.ID {
		t.Fatalf("claims=%d first=%+v", count, first)
	}
	if _, err := st.ClaimPRJob(ctx, repo, second.ID, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("overtook leased predecessor: %v", err)
	}
	recovered, err := st.ClaimPRJob(ctx, repo, j.ID, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first.State = "completed"
	if err := st.FinishPRJob(ctx, first); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired worker overwrote new claim: %v", err)
	}
	recovered.State = "completed"
	if err := st.FinishPRJob(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	got, err := st.ClaimPRJob(ctx, repo, second.ID, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got.State = "attention"
	got.Reason = "integrity_check_failed"
	if err := st.FinishPRJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryPRJob(ctx, repo, got.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimPRJob(ctx, repo, got.ID, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	other, err := st.ListPRJobs(ctx, domain.HashContent([]byte("other tenant")))
	if err != nil || len(other) != 0 {
		t.Fatalf("cross tenant=%+v %v", other, err)
	}
}
func TestFSJobLeasesAndReplay(t *testing.T) { checkPRJobs(t, NewFSStore(t.TempDir())) }
