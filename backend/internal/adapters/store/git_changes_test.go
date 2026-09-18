package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"sync"
	"testing"
	"time"
)

func checkGitChanges(t *testing.T, st interface {
	outbound.GitChangeStore
	outbound.MetadataStore
}) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	origin := "https://github.com/example/repo"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j := domain.GitChangeJob{RepoID: repo, GitOrigin: origin, Request: domain.GitChangeRequest{Target: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40)}, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	j.ID = domain.GitChangeID(repo, origin, j.Request)
	if _, err := st.EnqueueGitChange(ctx, j); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan domain.GitChangeJob, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := st.ClaimGitChange(ctx, repo, j.ID, now, time.Minute)
			if err == nil {
				claims <- got
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	var first domain.GitChangeJob
	count := 0
	for got := range claims {
		first = got
		count++
	}
	if count != 1 {
		t.Fatalf("parallel claims=%d", count)
	}
	again, err := st.EnqueueGitChange(ctx, j)
	if err != nil || again.State != "running" || again.Version != first.Version {
		t.Fatalf("replay reset lease: %+v %v", again, err)
	}
	if err := st.RetryGitChange(ctx, repo, j.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("manual retry stole active lease", err)
	}
	second, err := st.ClaimGitChange(ctx, repo, j.ID, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first.State = "attention"
	if err := st.FinishGitChange(ctx, first); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired worker overwrote new claim", err)
	}
	second.State = "completed"
	second.Result = &domain.GitReversalEvidence{Commit: j.Request.Commit, Target: j.Request.Target, Parent: j.Request.Target, Coverage: "full", Paths: []string{"feature.go"}, UnverifiedPaths: nil, OtherPaths: nil}
	bad := second
	bad.Request.Parent = strings.Repeat("c", 40)
	if err := st.FinishGitChange(ctx, bad); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("immutable request replaced", err)
	}
	if err := st.FinishGitChange(ctx, second); err != nil {
		t.Fatal(err)
	}
	second.State = "attention"
	second.Result = nil
	if err := st.FinishGitChange(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("lost ack overwrote receipt", err)
	}
	if err := st.RetryGitChange(ctx, repo, j.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	terminal, err := st.GetGitChange(ctx, repo, j.ID)
	if err != nil || terminal.State != "completed" || terminal.Result == nil {
		t.Fatalf("terminal receipt changed %+v %v", terminal, err)
	}
	for _, target := range []string{"c", "d", "e"} {
		other := j
		other.Request.Target = strings.Repeat(target, 40)
		other.ID = domain.GitChangeID(repo, origin, other.Request)
		if _, err := st.EnqueueGitChange(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		items, err := st.ListGitChanges(ctx, repo, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			if seen[item.ID] {
				t.Fatal("pagination duplicated", item.ID)
			}
			seen[item.ID] = true
			cursor = item.ID
		}
	}
	if len(seen) != 4 {
		t.Fatal("pagination lost records", seen)
	}
	foreign := domain.HashContent([]byte("other tenant"))
	if _, err := st.GetGitChange(ctx, foreign, j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross repo read", err)
	}
	if err := st.RetryGitChange(ctx, foreign, j.ID, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross repo retry", err)
	}
}
func TestFSGitChanges(t *testing.T) { st := NewFSStore(t.TempDir()); checkGitChanges(t, st) }
func TestFSGitChangesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	st := NewFSStore(dir)
	now := time.Now().UTC()
	repo := domain.HashContent([]byte(t.Name()))
	j := domain.GitChangeJob{RepoID: repo, GitOrigin: "https://github.com/example/repo", Request: domain.GitChangeRequest{Target: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40)}, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	j.ID = domain.GitChangeID(repo, j.GitOrigin, j.Request)
	if _, err := st.EnqueueGitChange(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	opened := NewFSStore(dir)
	got, err := opened.ClaimGitChange(context.Background(), repo, j.ID, now, time.Minute)
	if err != nil || got.Request != j.Request {
		t.Fatalf("restart %+v %v", got, err)
	}
}
