package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGitChangesDurableAcceptanceRetryAndNoHistoryMutation(t *testing.T) {
	core, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	origin := "https://github.com/example/repo"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	oid := func(c string) string { return strings.Repeat(c, 40) }
	r := domain.GitChangeRequest{Target: oid("a"), Commit: oid("b")}
	change := domain.GitPathChange{Path: "feature", Before: domain.GitEntry{OID: oid("1"), Mode: "100644"}, After: domain.GitEntry{OID: oid("2"), Mode: "100644"}}
	reader := fakeGitEvidence{t: t, origin: origin, ancestor: true, err: errors.New("offline"), deltas: map[string]domain.GitCommitDelta{r.Target: {Commit: r.Target, Parents: []string{oid("c")}, Parent: oid("c"), Complete: true, Changes: []domain.GitPathChange{change}}, r.Commit: {Commit: r.Commit, Parents: []string{r.Target}, Parent: r.Target, Complete: true, Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}}}}
	g, err := NewGitChanges(core, reader)
	if err != nil {
		t.Fatal(err)
	}
	job, err := g.Submit(ctx, repo, r)
	if err != nil || job.State != "waiting" {
		t.Fatalf("offline acceptance %+v %v", job, err)
	}
	if err := g.Process(ctx, 1); err != nil {
		t.Fatal(err)
	}
	job, err = g.Get(ctx, repo, job.ID)
	if err != nil || job.State != "retrying" || job.Result != nil {
		t.Fatalf("offline proof %+v %v", job, err)
	}
	reader.err = nil
	g, _ = NewGitChanges(core, reader)
	if err := g.Retry(ctx, repo, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.Process(ctx, 1); err != nil {
		t.Fatal(err)
	}
	done, err := g.Get(ctx, repo, job.ID)
	if err != nil || done.State != "completed" || done.Result == nil || done.Result.Coverage != "full" {
		t.Fatalf("recovery %+v %v", done, err)
	}
	replay, err := g.Submit(ctx, repo, r)
	if err != nil || !reflect.DeepEqual(replay, done) {
		t.Fatalf("idempotent replay %+v %v", replay, err)
	}
	events, err := core.ListHistory(ctx, repo)
	if err != nil || len(events) != 0 {
		t.Fatalf("evidence rewrote PR history %+v %v", events, err)
	}
	refs, err := core.ListRefs(ctx, repo)
	if err != nil || len(refs) != 0 {
		t.Fatalf("evidence moved refs %+v %v", refs, err)
	}
	// Capture the repository origin in the queued request. A later rebinding
	// cannot reuse a verified response obtained against a different repository.
	another := r
	another.Commit = oid("d")
	next, err := g.Submit(ctx, repo, another)
	if err != nil {
		t.Fatal(err)
	}
	core.meta = reboundGitRepo{MetadataStore: core.meta}
	claimed, err := st.ClaimGitChange(ctx, repo, next.ID, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.run(ctx, claimed); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("origin conflict", err)
	}
	next, err = g.Get(ctx, repo, next.ID)
	if err != nil || next.State != "attention" || next.Result != nil {
		t.Fatalf("origin rebound %+v %v", next, err)
	}
}

type reboundGitRepo struct{ outbound.MetadataStore }

func (s reboundGitRepo) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	r, err := s.MetadataStore.GetRepo(ctx, id)
	r.GitRemoteURL = "https://github.com/example/other"
	return r, err
}
