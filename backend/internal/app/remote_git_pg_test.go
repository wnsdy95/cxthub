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
	if err != nil || state.Remaining || !state.PullsComplete || !reflect.DeepEqual(state.Unreadable, []string{gone}) || !state.NextAt.Equal(clock.Add(domain.RemoteGitSyncEvery)) {
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
	// Each of the two passes moved the revision twice, when it ended and
	// when sessions were opened after it. Nothing a pass stores on its way
	// moves it: the commits of ten requests moved it no further.
	read, err := storage.RepositoryRevision(ctx, repo)
	if err != nil || read.Graph != before.Graph+4 || read.Pending != before.Pending || read.Evidence != before.Evidence {
		t.Fatalf("two passes moved the revision from %+v to %+v (%v)", before, read, err)
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
	if err != nil || state.RequestedAt == nil || !state.NextAt.Equal(clock.Add(domain.RemoteGitRequestSpacing)) {
		t.Fatalf("state after a request: %+v %v", state, err)
	}
	requested := *state.RequestedAt
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/main", "", strings.Repeat("1", 40), false, "remote-git-delivery"); err != nil {
		t.Fatal(err)
	}
	if state, err = storage.GetRemoteGitState(ctx, repo); err != nil || !state.RequestedAt.After(requested) {
		t.Fatalf("state after a push: %+v %v", state, err)
	}
	// The request changes nothing a page shows; the delivered push is an
	// observation of the branch ledger, which moves the evidence counter.
	if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after.Graph != read.Graph || after.Pending != read.Pending || after.Evidence != read.Evidence+1 {
		t.Fatalf("a request and a delivered push moved the revision from %+v to %+v (%v)", read, after, err)
	}
	clock = clock.Add(domain.RemoteGitRequestSpacing)
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil || !reflect.DeepEqual(provider.requests, []string{"default", "heads:1", "heads:2", "pulls:1"}) {
		t.Fatalf("the pass that was asked for: %v %v", provider.requests, err)
	}
}

// A look at the page from the top asks for a read once the last pass started
// five minutes ago or more, through the store alone: no repository
// transaction is open around the request, and a look is no write of the
// repository, so its revision stays.
func TestPGRemoteGitLookAsksForARead(t *testing.T) {
	service, storage, repo := remoteGitPG(t)
	ctx := systemTestContext()
	provider := newRemoteGitTestProvider(100)
	provider.heads["main"] = provider.chain(3, "")
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	worker.now = func() time.Time { return clock }
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	read, err := storage.GetRemoteGitState(ctx, repo)
	if err != nil || read.RequestedAt != nil || !read.NextAt.Equal(clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state after the pass: %+v %v", read, err)
	}
	revision, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC().Add(-time.Millisecond)
	page, err := service.ReadRemoteGit(ctx, repo, domain.RemoteCommitCursor{}, 2)
	after := time.Now().UTC().Add(time.Millisecond)
	if err != nil || !page.SyncedAt.Equal(clock) || len(page.Commits) != 2 {
		t.Fatalf("the page of the look: %+v %v", page, err)
	}
	asked, err := storage.GetRemoteGitState(ctx, repo)
	if err != nil || asked.RequestedAt == nil || asked.RequestedAt.Before(before) || asked.RequestedAt.After(after) || !asked.NextAt.Equal(*asked.RequestedAt) {
		t.Fatalf("state after the look: %+v %v, between %s and %s", asked, err, before, after)
	}
	// A pass is due now: another look, and a page further down, ask nothing.
	for _, cursor := range []domain.RemoteCommitCursor{{}, *page.Next} {
		if _, err := service.ReadRemoteGit(ctx, repo, cursor, 2); err != nil {
			t.Fatal(err)
		}
		if again, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(again, asked) {
			t.Fatalf("a look after the one that asked changed the state:\n got %+v\nwant %+v (%v)", again, asked, err)
		}
	}
	if now, err := storage.RepositoryRevision(ctx, repo); err != nil || !reflect.DeepEqual(now, revision) {
		t.Fatalf("a look moved the revision from %+v to %+v (%v)", revision, now, err)
	}
	// The worker finds the pass due and reads.
	provider.requests, clock = nil, time.Now().UTC().Truncate(time.Second).Add(time.Second)
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil || len(provider.requests) == 0 {
		t.Fatalf("the pass the look asked for: %v %v", provider.requests, err)
	}
	// A look right after it asks nothing.
	finished, err := storage.GetRemoteGitState(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadRemoteGit(ctx, repo, domain.RemoteCommitCursor{}, 2); err != nil {
		t.Fatal(err)
	}
	if state, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(state, finished) {
		t.Fatalf("a look right after a pass changed the state:\n got %+v\nwant %+v (%v)", state, finished, err)
	}
}

// On PostgreSQL a request for a read is announced by the database itself when
// the transaction that stored it is committed, to every connection that
// listens: the reader hears of it in whichever process it runs, and what it
// then reads through its own connection already holds the request.
func TestPGRemoteGitRequestsAreHeardWhenTheyAreCommitted(t *testing.T) {
	service, storage, repo := remoteGitPG(t)
	ctx := systemTestContext()
	provider := newRemoteGitTestProvider(100)
	provider.heads["main"] = provider.chain(2, "")
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	worker.now = func() time.Time { return clock }
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	stored, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	heard := make(chan domain.RemoteGitState, 16)
	stop, err := storage.ListenRemoteGitRequests(func(asked domain.ContentHash) {
		// Other tests' repositories are announced on the same channel.
		if asked != repo {
			return
		}
		state, err := storage.GetRemoteGitState(ctx, asked)
		if err != nil {
			t.Errorf("the state of a repository that was announced: %v", err)
		}
		heard <- state
	})
	if err != nil {
		t.Fatal(err)
	}
	hear := func(what string) domain.RemoteGitState {
		t.Helper()
		select {
		case state := <-heard:
			return state
		case <-time.After(10 * time.Second):
			t.Fatalf("%s was not heard", what)
			return domain.RemoteGitState{}
		}
	}
	silence := func(what string) {
		t.Helper()
		select {
		case state := <-heard:
			t.Fatalf("%s was heard: %+v", what, state)
		case <-time.After(300 * time.Millisecond):
		}
	}
	silence("nothing")
	last := time.Time{}
	for _, request := range []struct {
		name string
		ask  func() error
	}{
		{"a member's request", func() error { return service.RequestRemoteGitRefresh(ctx, repo) }},
		{"a push the host delivered", func() error {
			_, err := service.ObserveRemotePush(ctx, stored.GitRemoteURL, "refs/heads/main", "", strings.Repeat("1", 40), false, "heard-when-committed")
			return err
		}},
		{"a look at an old read", func() error {
			// The requests before made a pass due; the look asks only when
			// none is, so the state is put back to that of the pass alone.
			state := mustRemoteGitState(t, storage, repo)
			state.RequestedAt = nil
			if err := storage.FinishRemoteGitSync(ctx, state.Ended(clock, false, false)); err != nil {
				return err
			}
			_, err := service.ReadRemoteGit(ctx, repo, domain.RemoteCommitCursor{}, 2)
			return err
		}},
	} {
		if err := request.ask(); err != nil {
			t.Fatalf("%s: %v", request.name, err)
		}
		state := hear(request.name)
		if state.RequestedAt == nil || state.RequestedAt.Before(last) || state.RequestedAt.Equal(last) {
			t.Fatalf("%s was heard before its request could be read: %+v", request.name, state)
		}
		last = *state.RequestedAt
		silence("a second announcement of " + request.name)
	}
	// A request whose transaction is not committed is announced to nobody.
	if err := storage.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := storage.RequestRemoteGitSync(tx, repo, time.Now().UTC()); err != nil {
			return err
		}
		return errors.New("the transaction is given up")
	}); err == nil {
		t.Fatal("the transaction was committed")
	}
	silence("a request that was rolled back")
	// After stop nothing is told, and stop returns.
	stop()
	if err := service.RequestRemoteGitRefresh(ctx, repo); err != nil {
		t.Fatal(err)
	}
	silence("a request after listening stopped")
	// The reader's own loop listens the same way: told of a request, it reads.
	read := make(chan struct{}, 4)
	provider.before = func(request string) {
		if request == "pulls:1" {
			read <- struct{}{}
		}
	}
	clock = time.Now().UTC().Truncate(time.Second).Add(time.Second)
	running, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() { defer close(ended); worker.Run(running) }()
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not read the repository that was due when it started")
	}
	clock = clock.Add(domain.RemoteGitRequestSpacing)
	if err := service.RequestRemoteGitRefresh(ctx, repo); err != nil {
		t.Fatal(err)
	}
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not read when a request was committed")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop went on after its context ended")
	}
}

func mustRemoteGitState(t *testing.T, storage *store.PostgresStore, repo domain.ContentHash) domain.RemoteGitState {
	t.Helper()
	state, err := storage.GetRemoteGitState(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// On PostgreSQL the mark a page follows is read in a snapshot, and the end of
// a read of the Git host moves it in the transaction that stores that end.
func TestPGViewRevisionMovesWhenAReadOfTheGitHostEnds(t *testing.T) {
	service, storage, repo := remoteGitPG(t)
	ctx := systemTestContext()
	provider := newRemoteGitTestProvider(100)
	provider.heads["main"] = provider.chain(2, "")
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	worker.now = func() time.Time { return clock }
	mark := func() string {
		t.Helper()
		revision, err := service.ViewRevision(ctx, repo)
		if err != nil || revision == "" {
			t.Fatalf("the mark: %q %v", revision, err)
		}
		return revision
	}
	first := mark()
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	read := mark()
	if read == first {
		t.Fatalf("the end of a read left the mark at %q", first)
	}
	// A look at the old read asks for another and moves nothing by itself.
	if _, err := service.ReadRemoteGit(ctx, repo, domain.RemoteCommitCursor{}, 2); err != nil {
		t.Fatal(err)
	}
	if state, err := storage.GetRemoteGitState(ctx, repo); err != nil || state.RequestedAt == nil {
		t.Fatalf("the look did not ask: %+v %v", state, err)
	}
	if err := service.RequestRemoteGitRefresh(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if after := mark(); after != read {
		t.Fatalf("a look and a request moved the mark from %q to %q", read, after)
	}
	clock = time.Now().UTC().Truncate(time.Second).Add(time.Second)
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if after := mark(); after == read || after == first {
		t.Fatalf("the read that was asked for left the mark at %q", after)
	}
	if _, err := service.ViewRevision(ctx, hh("a repository nobody registered "+t.Name())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the mark of a repository this server does not hold: %v", err)
	}
	// What is no repository identifier is refused as such on this store too.
	if _, err := service.ViewRevision(ctx, "not a repository"); !errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the mark of what is no repository: %v", err)
	}
}
