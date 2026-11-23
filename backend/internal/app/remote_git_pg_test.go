//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// remoteGitPG is the PostgreSQL fixture with a repository of its own origin.
// A push is delivered to every repository of an origin, so a repository that
// shared the fixture's usual origin would see the deliveries, and the
// refusals, of the repositories other tests left in the database.
func remoteGitPG(t *testing.T) (*Service, *store.PostgresStore, domain.ContentHash) {
	t.Helper()
	service, storage, _ := collaborationPG(t)
	unique := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	repo := hh(unique)
	origin := "https://github.com/example/" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(unique)) + ".git"
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	return service, storage, repo
}

// A pass stores what it reads through repository transactions. The pool of
// this fixture holds two connections, so a store call that left its
// transaction would show.
func TestPGRemoteGitPassStoresWhatItReads(t *testing.T) {
	service, storage, repo := remoteGitPG(t)
	ctx := systemTestContext()
	stored, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	origin := stored.GitRemoteURL
	provider := newRemoteGitTestProvider(2)
	main := provider.chain(5, "")
	topic := provider.chain(2, main)
	provider.heads["main"], provider.heads["topic"] = main, topic
	provider.pull(1, 50, topic, remoteGitTestSHA(6), topic)
	gone := remoteGitTestSHA(0x900)
	provider.pull(2, 40, provider.commit(gone), remoteGitTestSHA(8))
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := remoteGitTestTime(100000)
	worker.now = func() time.Time { return clock }
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	// A budget of eight requests is gone when both branches are read: the
	// pull requests are left for the next pass.
	worker.budget = 8
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "heads:1", "heads:2", "commits:5:1", "commits:5:2", "commits:5:3", "commits:7:1", "commits:7:2"}
	if !reflect.DeepEqual(provider.requests, want) {
		t.Fatalf("first pass:\n got %v\nwant %v", provider.requests, want)
	}
	state, err := storage.GetRemoteGitState(ctx, repo)
	if err != nil || !state.Remaining || !state.SyncedAt.Equal(clock) || !state.NextAt.Equal(clock.Add(time.Minute)) || state.PullsComplete || state.DefaultBranch != "main" || state.GitOrigin != origin {
		t.Fatalf("state after the first pass: %+v %v", state, err)
	}
	if branches, err := storage.ListRemoteBranches(ctx, repo); err != nil || !reflect.DeepEqual(branches, []domain.RemoteBranch{{Name: "main", SHA: main}, {Name: "topic", SHA: topic}}) {
		t.Fatalf("branches: %+v %v", branches, err)
	}

	// The next pass reads the pull requests, and the parent one of them
	// names that the provider does not hold.
	worker.budget, provider.requests, clock = remoteGitPassBudget, nil, clock.Add(time.Minute)
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	want = []string{"default", "heads:1", "heads:2", "pulls:1", "pull:1:1", "pull:1:2", "pull:2:1", "pulls:2", "commits:900:1"}
	if !reflect.DeepEqual(provider.requests, want) {
		t.Fatalf("second pass:\n got %v\nwant %v", provider.requests, want)
	}
	state, err = storage.GetRemoteGitState(ctx, repo)
	if err != nil || state.Remaining || !state.PullsComplete || !reflect.DeepEqual(state.Unreadable, []string{gone}) || !state.NextAt.Equal(clock.Add(5*time.Minute)) {
		t.Fatalf("state after the second pass: %+v %v", state, err)
	}
	page, err := service.ReadRemoteGit(ctx, repo, domain.RemoteCommitCursor{}, 3)
	if err != nil || len(page.Commits) != 3 || page.Commits[0].SHA != remoteGitTestSHA(8) || !reflect.DeepEqual(page.Commits[1], provider.commits[topic]) || page.Next == nil ||
		len(page.Branches) != 2 || len(page.Pulls) != 2 || page.DefaultBranch != "main" || !page.SyncedAt.Equal(clock) {
		t.Fatalf("what the service reads: %+v %v", page, err)
	}
	if !reflect.DeepEqual(page.Pulls[1], provider.pulls[1]) || !reflect.DeepEqual(page.Pulls[0].Commits, []string{remoteGitTestSHA(8)}) {
		t.Fatalf("pull requests:\n got %+v\nwant %+v", page.Pulls, provider.pulls)
	}
	rest, err := service.ReadRemoteGit(ctx, repo, *page.Next, domain.RemoteGitPageLimit)
	if err != nil || len(rest.Commits) != 5 || rest.Next != nil || len(rest.Branches) != 0 || rest.Commits[4].SHA != remoteGitTestSHA(1) {
		t.Fatalf("the following page: %+v %v", rest, err)
	}
	if after, err := storage.RepositoryRevision(ctx, repo); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("revision moved from %+v to %+v (%v)", before, after, err)
	}

	// No pass is due; a member's request and a delivered push each pull the
	// next one closer, in the transaction that records them.
	provider.requests = nil
	if err := worker.SyncRemoteGit(context.Background(), repo); !errors.Is(err, domain.ErrNotFound) || len(provider.requests) != 0 {
		t.Fatalf("a pass before it is due: %v %v", provider.requests, err)
	}
	if err := service.RequestRemoteGitRefresh(ctx, repo); err != nil {
		t.Fatal(err)
	}
	state, err = storage.GetRemoteGitState(ctx, repo)
	if err != nil || state.RequestedAt == nil || !state.NextAt.Equal(clock.Add(30*time.Second)) {
		t.Fatalf("state after a request: %+v %v", state, err)
	}
	requested := *state.RequestedAt
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/main", "", strings.Repeat("1", 40), false, "remote-git-delivery"); err != nil {
		t.Fatal(err)
	}
	if state, err = storage.GetRemoteGitState(ctx, repo); err != nil || !state.RequestedAt.After(requested) {
		t.Fatalf("state after a push: %+v %v", state, err)
	}
	if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after.Graph != before.Graph {
		t.Fatalf("a request moved the graph revision from %+v to %+v (%v)", before, after, err)
	}
	clock = clock.Add(30 * time.Second)
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil || !reflect.DeepEqual(provider.requests, []string{"default", "heads:1", "heads:2", "pulls:1"}) {
		t.Fatalf("the pass that was asked for: %v %v", provider.requests, err)
	}
}
