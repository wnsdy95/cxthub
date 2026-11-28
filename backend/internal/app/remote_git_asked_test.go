package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// remoteGitAskedFixture is a repository whose first read started ago before
// now, with the reader listening to the store as its loop does. The service
// goes by the time it is, so the reader's clock is set near it.
func remoteGitAskedFixture(t *testing.T, ago time.Duration) *remoteGitTestFixture {
	t.Helper()
	fixture := newRemoteGitTestFixture(t, 100)
	fixture.provider.heads["main"] = fixture.provider.chain(3, "")
	fixture.clock = time.Now().UTC().Add(-ago).Truncate(time.Second)
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	stop, err := fixture.storage.ListenRemoteGitRequests(fixture.sync.Ask)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return fixture
}

// told takes what the reader was told of out of its queue.
func (fixture *remoteGitTestFixture) told() []domain.ContentHash {
	var repos []domain.ContentHash
	for {
		select {
		case repo := <-fixture.sync.asked:
			repos = append(repos, repo)
		default:
			return repos
		}
	}
}

// A read that is asked for is told to the reader that listens to the store,
// whoever asked: a member, a look at what was read, and a push the Git host
// delivered. A request that was refused is told to nobody.
func TestRemoteGitReaderIsToldOfEveryReadThatIsAskedFor(t *testing.T) {
	member, viewer := anchoredActor("member-a"), anchoredActor("viewer")
	only := func(t *testing.T, fixture *remoteGitTestFixture, what string) {
		t.Helper()
		if told := fixture.told(); !reflect.DeepEqual(told, []domain.ContentHash{fixture.repo}) {
			t.Fatalf("%s: the reader was told of %v", what, told)
		}
		if state := fixture.state(t); state.RequestedAt == nil {
			t.Fatalf("%s: the reader was told of a request the store does not hold: %+v", what, state)
		}
	}

	fixture := remoteGitAskedFixture(t, 10*time.Minute)
	if told := fixture.told(); len(told) != 0 {
		t.Fatalf("the reader was told of %v before anything was asked", told)
	}
	if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
		t.Fatal(err)
	}
	only(t, fixture, "a member's request")

	fixture = remoteGitAskedFixture(t, 10*time.Minute)
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 2); err != nil {
		t.Fatal(err)
	}
	only(t, fixture, "a look at an old read")
	// Another look finds a pass due and asks nothing: nobody is told twice.
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 2); err != nil || len(fixture.told()) != 0 {
		t.Fatalf("a second look told the reader again: %v", err)
	}

	fixture = remoteGitAskedFixture(t, 10*time.Minute)
	if n, err := fixture.service.ObserveRemotePush(systemTestContext(), remoteGitTestOrigin, "refs/heads/main", "", strings.Repeat("1", 40), false, "delivery-1"); err != nil || n != 1 {
		t.Fatalf("the delivery: %d %v", n, err)
	}
	only(t, fixture, "a push the host delivered")

	// What was not stored is told to nobody.
	fixture = remoteGitAskedFixture(t, 10*time.Minute)
	if err := fixture.service.RequestRemoteGitRefresh(viewer, fixture.repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer's request: %v", err)
	}
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a look that is refused: %v", err)
	}
	fixture.storage.refusal = errors.New("the store is unavailable")
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 2); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err == nil {
		t.Fatal("a request the store refused succeeded")
	}
	fixture.storage.refusal = nil
	fixture.setOrigin("")
	if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a request for a repository without an origin: %v", err)
	}
	if told := fixture.told(); len(told) != 0 {
		t.Fatalf("the reader was told of %v, and nothing was stored", told)
	}
	// A look a minute after a pass asks nothing and tells nobody.
	fixture = remoteGitAskedFixture(t, time.Minute)
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 2); err != nil || len(fixture.told()) != 0 {
		t.Fatalf("a look at a recent read told the reader: %v", err)
	}
}

// A delivery the GitHub worker applies is told to the reader like any request:
// when the delivery is applied, not when it is only received.
func TestRemoteGitReaderIsToldWhenTheGitHubWorkerAppliesADelivery(t *testing.T) {
	connections, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Hour)
	claimed, err := storage.ClaimRemoteGitSync(ctx, repo.ID, repo.GitRemoteURL, started, remoteGitLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishRemoteGitSync(ctx, claimed.Ended(started, false, false)); err != nil {
		t.Fatal(err)
	}
	var told []domain.ContentHash
	stop, err := storage.ListenRemoteGitRequests(func(asked domain.ContentHash) {
		// The request is stored by the time the reader is told.
		if state, err := storage.GetRemoteGitState(ctx, asked); err != nil || state.RequestedAt == nil {
			t.Errorf("told before the request was stored: %+v %v", state, err)
		}
		told = append(told, asked)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	body := []byte(`{"installation":{"id":789},"repository":{"id":91},"ref":"refs/heads/topic","before":"` + strings.Repeat("0", 40) + `","after":"` + strings.Repeat("c", 40) + `"}`)
	if err := connections.Receive(ctx, "asked-push", "push", body); err != nil {
		t.Fatal(err)
	}
	if len(told) != 0 {
		t.Fatalf("the reader was told of %v when the delivery was only received", told)
	}
	if err := githubWorkerOf(connections).deliver(ctx, githubHash("asked-push")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(told, []domain.ContentHash{repo.ID}) {
		t.Fatalf("the reader was told of %v after the delivery was applied", told)
	}
}

// The reader answers what it was told of by running the pass, and comes back
// by itself for a request that it may not read yet.
func TestRemoteGitReaderAnswersAtOnceAndComesBackForWhatCameTooSoon(t *testing.T) {
	member := anchoredActor("member-a")
	type timer struct {
		wait time.Duration
		run  func()
	}
	prepare := func(t *testing.T, ago time.Duration) (*remoteGitTestFixture, *[]timer) {
		t.Helper()
		fixture := remoteGitAskedFixture(t, ago)
		timers := &[]timer{}
		fixture.sync.later = func(wait time.Duration, run func()) { *timers = append(*timers, timer{wait, run}) }
		return fixture, timers
	}
	answer := func(fixture *remoteGitTestFixture) []string {
		fixture.provider.requests = nil
		for _, repo := range fixture.told() {
			fixture.sync.answer(context.Background(), repo)
		}
		return fixture.provider.requests
	}

	t.Run("a request long after the last pass", func(t *testing.T) {
		fixture, timers := prepare(t, 10*time.Minute)
		fixture.provider.heads["main"] = fixture.provider.chain(1, fixture.provider.heads["main"])
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		fixture.clock = time.Now().UTC().Add(time.Second)
		if requests := answer(fixture); !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:4:1", "pulls:1"}) {
			t.Fatalf("the answer asked the provider %v", requests)
		}
		state := fixture.state(t)
		if !state.SyncedAt.Equal(fixture.clock) || !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
			t.Fatalf("state after the answer: %+v", state)
		}
		// The next pass is hours away: nothing is asked again, now or later.
		if len(*timers) != 0 || len(fixture.told()) != 0 {
			t.Fatalf("after a pass with nothing more asked: %d timers", len(*timers))
		}
	})

	t.Run("a request right after a pass", func(t *testing.T) {
		// The last pass started four seconds ago, so the request may be read
		// ten seconds after that start, in about six seconds.
		fixture, timers := prepare(t, 4*time.Second)
		started := fixture.clock
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		fixture.clock = time.Now().UTC()
		if requests := answer(fixture); len(requests) != 0 {
			t.Fatalf("a pass ran %s after the one before it: %v", fixture.clock.Sub(started), requests)
		}
		if len(*timers) != 1 {
			t.Fatalf("%d timers after a request that came too soon", len(*timers))
		}
		due := started.Add(domain.RemoteGitRequestSpacing)
		if wait := (*timers)[0].wait; wait <= 0 || wait > domain.RemoteGitRequestSpacing || wait != due.Sub(fixture.clock) {
			t.Fatalf("the reader comes back in %s; the pass is due in %s", wait, due.Sub(fixture.clock))
		}
		// When the time has come the reader is told again and reads.
		(*timers)[0].run()
		*timers = nil
		fixture.clock = due
		if requests := answer(fixture); len(requests) == 0 {
			t.Fatal("the reader did not read when it came back")
		}
		if state := fixture.state(t); !state.SyncedAt.Equal(due) || len(*timers) != 0 {
			t.Fatalf("state after it came back: %+v, %d timers", state, len(*timers))
		}
	})

	t.Run("a request while a pass runs, late in it", func(t *testing.T) {
		// The pass takes longer than the spacing; the request that came
		// during it is due as soon as the pass has ended.
		fixture, timers := prepare(t, 10*time.Minute)
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		fixture.clock = time.Now().UTC().Add(time.Second)
		start := fixture.clock
		fixture.provider.before = func(request string) {
			if request == "pulls:1" {
				fixture.clock = start.Add(domain.RemoteGitRequestSpacing + 5*time.Second)
				if err := fixture.storage.RequestRemoteGitSync(context.Background(), fixture.repo, fixture.clock); err != nil {
					t.Fatal(err)
				}
				// The store tells of the request at once. A reader that
				// answered it now would find the repository claimed by the
				// pass that runs, and leave it.
				if told := fixture.told(); !reflect.DeepEqual(told, []domain.ContentHash{fixture.repo}) {
					t.Errorf("the store told of %v when the request was stored", told)
				}
			}
		}
		if requests := answer(fixture); len(requests) == 0 {
			t.Fatal("the pass did not run")
		}
		fixture.provider.before = nil
		if state := fixture.state(t); !state.NextAt.Equal(start.Add(domain.RemoteGitRequestSpacing+5*time.Second)) || !state.SyncedAt.Equal(start) {
			t.Fatalf("state after a request during the pass: %+v", state)
		}
		if told := fixture.told(); !reflect.DeepEqual(told, []domain.ContentHash{fixture.repo}) || len(*timers) != 0 {
			t.Fatalf("after a pass with a request during it the reader was told of %v, with %d timers", told, len(*timers))
		}
	})

	t.Run("a pass that fails", func(t *testing.T) {
		// It is tried again by its own rule, two minutes later: the look for
		// due repositories does that, not a timer of the answer.
		fixture, timers := prepare(t, 10*time.Minute)
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		fixture.provider.failures["default"] = errors.New("the provider is unreachable")
		fixture.clock = time.Now().UTC().Add(time.Second)
		if requests := answer(fixture); !reflect.DeepEqual(requests, []string{"default"}) {
			t.Fatalf("the answer asked the provider %v", requests)
		}
		if state := fixture.state(t); !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncRetry)) {
			t.Fatalf("state after the failure: %+v", state)
		}
		if len(*timers) != 0 || len(fixture.told()) != 0 {
			t.Fatalf("after a failed pass: %d timers", len(*timers))
		}
	})

	t.Run("a repository the server may not read", func(t *testing.T) {
		// No installation was told to read it: that lasts until a maintainer
		// connects it, so the next pass nobody asked for comes after the
		// fallback time, not two minutes later, and nothing counts as read.
		fixture, timers := prepare(t, 10*time.Minute)
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		before := fixture.state(t)
		fixture.provider.failures["default"] = fmt.Errorf("%w: %w", outbound.ErrGitRepositoryNotConnected, domain.ErrForbidden)
		fixture.clock = time.Now().UTC().Add(time.Second)
		if requests := answer(fixture); !reflect.DeepEqual(requests, []string{"default"}) {
			t.Fatalf("the answer asked the provider %v", requests)
		}
		state := fixture.state(t)
		if !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) || !reflect.DeepEqual(state.SyncedAt, before.SyncedAt) {
			t.Fatalf("state after a pass the server may not make: %+v, before %+v", state, before)
		}
		if len(*timers) != 0 || len(fixture.told()) != 0 {
			t.Fatalf("after a pass the server may not make: %d timers", len(*timers))
		}
		// Asking again still brings the pass closer, as for any repository.
		fixture.clock = fixture.clock.Add(time.Minute)
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		if state := fixture.state(t); !state.NextAt.Before(fixture.clock.Add(time.Minute)) {
			t.Fatalf("a request after an unconnected pass did not bring it closer: %+v", state)
		}
	})

	t.Run("a repository the reader cannot answer for", func(t *testing.T) {
		fixture, timers := prepare(t, 10*time.Minute)
		// Never read: no state to look at, and nothing comes back.
		fixture.sync.answer(context.Background(), hh("a repository nobody registered"))
		// Another process holds the claim: this one leaves it alone.
		if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
			t.Fatal(err)
		}
		fixture.clock = time.Now().UTC().Add(time.Second)
		if _, err := fixture.storage.ClaimRemoteGitSync(context.Background(), fixture.repo, remoteGitTestOrigin, fixture.clock, remoteGitLease); err != nil {
			t.Fatal(err)
		}
		if requests := answer(fixture); len(requests) != 0 || len(*timers) != 0 || len(fixture.told()) != 0 {
			t.Fatalf("a claimed repository: requests %v, %d timers", requests, len(*timers))
		}
	})
}

// Telling the reader never waits, and a reader that is absent is told nothing.
func TestRemoteGitAskNeverWaits(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range remoteGitAskedQueue + 10 {
			fixture.sync.Ask(fixture.repo)
		}
		var absent *RemoteGitSync
		absent.Ask(fixture.repo)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("telling the reader waited for a full queue")
	}
	if held := len(fixture.told()); held != remoteGitAskedQueue {
		t.Fatalf("the queue held %d requests, it takes %d", held, remoteGitAskedQueue)
	}
}

// Run listens to the store and reads a repository as soon as a request for it
// is stored, without waiting for its look for due repositories, which comes
// every thirty seconds.
func TestRemoteGitRunAnswersWithoutWaitingForItsNextLook(t *testing.T) {
	for name, run := range map[string]func(*RemoteGitSync, context.Context){
		"Run": (*RemoteGitSync).Run,
	} {
		t.Run(name, func(t *testing.T) {
			// Nothing but the loop itself listens to the store here.
			fixture := newRemoteGitTestFixture(t, 100)
			fixture.provider.heads["main"] = fixture.provider.chain(3, "")
			fixture.clock = time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
			if _, err := fixture.pass(t); err != nil {
				t.Fatal(err)
			}
			fixture.clock = time.Now().UTC().Add(time.Second)
			read := make(chan struct{}, 8)
			fixture.provider.before = func(request string) {
				if request == "pulls:1" {
					read <- struct{}{}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			ended := make(chan struct{})
			go func() { defer close(ended); run(fixture.sync, ctx) }()
			// Nothing is due: the loop reads nothing by itself. Its first look
			// for due repositories comes after it began to listen.
			select {
			case <-read:
				t.Fatal("a pass ran before anything was asked")
			case <-time.After(300 * time.Millisecond):
			}
			if err := fixture.service.RequestRemoteGitRefresh(anchoredActor("member-a"), fixture.repo); err != nil {
				t.Fatal(err)
			}
			select {
			case <-read:
			case <-time.After(10 * time.Second):
				t.Fatal("the read that was asked for did not run")
			}
			cancel()
			select {
			case <-ended:
			case <-time.After(10 * time.Second):
				t.Fatal("the loop went on after its context ended")
			}
			// The loop that ended listens no more.
			fixture.told()
			if err := fixture.storage.RequestRemoteGitSync(context.Background(), fixture.repo, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if told := fixture.told(); len(told) != 0 {
				t.Fatalf("a loop that ended was told of %v", told)
			}
		})
	}
}

// The look for due repositories answers like a request does: a read that was
// asked for while its pass ran is not left for the look thirty seconds later.
func TestRemoteGitLookForDueRepositoriesComesBackLikeAnAnswer(t *testing.T) {
	fixture := remoteGitAskedFixture(t, 10*time.Minute)
	// The pass nobody asked for is due.
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	start := fixture.clock
	fixture.provider.before = func(request string) {
		if request == "pulls:1" {
			fixture.clock = start.Add(domain.RemoteGitRequestSpacing + 5*time.Second)
			if err := fixture.storage.RequestRemoteGitSync(context.Background(), fixture.repo, fixture.clock); err != nil {
				t.Fatal(err)
			}
			// What the store tells of now finds the repository claimed.
			fixture.told()
		}
	}
	if err := fixture.sync.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := fixture.state(t); !state.SyncedAt.Equal(start) {
		t.Fatalf("the due pass did not run: %+v", state)
	}
	if told := fixture.told(); !reflect.DeepEqual(told, []domain.ContentHash{fixture.repo}) {
		t.Fatalf("after a due pass with a request during it the reader was told of %v", told)
	}
}

// A pull request the Git host delivered moves no branch. It asks for a read
// of every repository of its origin, which the reader is told of like a push.
func TestRemoteGitReaderIsToldOfADeliveredPullRequest(t *testing.T) {
	fixture := remoteGitAskedFixture(t, 10*time.Minute)
	for _, origin := range []string{remoteGitTestOrigin, remoteGitTestOrigin + ".git", "git@github.com:example/project.git"} {
		before := fixture.state(t)
		if n, err := fixture.service.ObserveRemotePullRequest(systemTestContext(), origin); err != nil || n != 1 {
			t.Fatalf("a pull request of %s: %d repositories, %v", origin, n, err)
		}
		state := fixture.state(t)
		if told := fixture.told(); !reflect.DeepEqual(told, []domain.ContentHash{fixture.repo}) || state.RequestedAt == nil || (before.RequestedAt != nil && !state.RequestedAt.After(*before.RequestedAt)) {
			t.Fatalf("a pull request of %s: the reader was told of %v; state %+v", origin, told, state)
		}
	}
	// Another origin, and none, are no repository of this server.
	asked := fixture.state(t)
	for _, origin := range []string{"https://github.com/example/another", ""} {
		if n, err := fixture.service.ObserveRemotePullRequest(systemTestContext(), origin); err != nil || n != 0 {
			t.Fatalf("a pull request of %q: %d repositories, %v", origin, n, err)
		}
	}
	if told := fixture.told(); len(told) != 0 || !reflect.DeepEqual(fixture.state(t), asked) {
		t.Fatalf("a pull request of another origin: the reader was told of %v", told)
	}
	// A repository whose origin changed since the list was read is left alone.
	fixture.setOrigin("https://github.com/example/moved")
	if n, err := fixture.service.ObserveRemotePullRequest(systemTestContext(), remoteGitTestOrigin); err != nil || n != 0 || len(fixture.told()) != 0 {
		t.Fatalf("a pull request of an origin the repository no longer has: %d repositories, %v", n, err)
	}
}
