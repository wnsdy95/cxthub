package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const remoteGitTestOrigin = "https://github.com/example/project"

func remoteGitTestSHA(number int) string { return fmt.Sprintf("%040x", number) }

func remoteGitTestTime(seconds int) time.Time {
	return time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
}

// remoteGitTestProvider is a hosting provider that holds a repository in
// memory and answers a page at a time, as the real one does.
type remoteGitTestProvider struct {
	pageSize      int
	defaultBranch string
	heads         map[string]string
	commits       map[string]domain.RemoteCommit
	pulls         map[int]domain.RemotePull
	requests      []string
	origins       map[string]int
	failures      map[string]error
	before        func(request string)
	next          int
}

func newRemoteGitTestProvider(pageSize int) *remoteGitTestProvider {
	return &remoteGitTestProvider{pageSize: pageSize, defaultBranch: "main", heads: map[string]string{}, commits: map[string]domain.RemoteCommit{},
		pulls: map[int]domain.RemotePull{}, origins: map[string]int{}, failures: map[string]error{}}
}

// commit adds a commit on top of parents and returns its number. Each commit
// is one second younger than the one added before it.
func (provider *remoteGitTestProvider) commit(parents ...string) string {
	provider.next++
	sha := remoteGitTestSHA(provider.next)
	provider.commits[sha] = domain.RemoteCommit{SHA: sha, Parents: append([]string{}, parents...), AuthorName: "Author", AuthorEmail: "author@example.com",
		AuthoredAt: remoteGitTestTime(provider.next), CommittedAt: remoteGitTestTime(provider.next), Subject: fmt.Sprintf("commit %d", provider.next)}
	return sha
}

// chain adds count commits, each on top of the one before, and returns the last.
func (provider *remoteGitTestProvider) chain(count int, parent string) string {
	for index := 0; index < count; index++ {
		if parent == "" {
			parent = provider.commit()
		} else {
			parent = provider.commit(parent)
		}
	}
	return parent
}

func (provider *remoteGitTestProvider) pull(number, updated int, head string, commits ...string) {
	provider.pulls[number] = domain.RemotePull{Number: number, Title: fmt.Sprintf("Pull request %d", number), State: domain.RemotePullOpen,
		HeadRef: fmt.Sprintf("topic-%d", number), HeadSHA: head, BaseRef: "main", Commits: commits, UpdatedAt: remoteGitTestTime(updated)}
}

func (provider *remoteGitTestProvider) request(origin, request string) error {
	provider.requests = append(provider.requests, request)
	provider.origins[origin]++
	if provider.before != nil {
		provider.before(request)
	}
	return provider.failures[request]
}

func remoteGitTestPage[T any](all []T, page, size int) ([]T, bool) {
	first := (page - 1) * size
	if first >= len(all) {
		return []T{}, false
	}
	last := min(first+size, len(all))
	return append([]T{}, all[first:last]...), last-first == size
}

func (provider *remoteGitTestProvider) ReadRemoteDefaultBranch(_ context.Context, origin string) (string, error) {
	if err := provider.request(origin, "default"); err != nil {
		return "", err
	}
	return provider.defaultBranch, nil
}

func (provider *remoteGitTestProvider) ListGitHeads(_ context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if err := provider.request(origin, fmt.Sprintf("heads:%d", page)); err != nil {
		return nil, false, err
	}
	heads := []outbound.GitHead{}
	for name, sha := range provider.heads {
		heads = append(heads, outbound.GitHead{Ref: "refs/heads/" + name, Commit: sha})
	}
	sort.Slice(heads, func(left, right int) bool { return heads[left].Ref < heads[right].Ref })
	listed, more := remoteGitTestPage(heads, page, provider.pageSize)
	return listed, more, nil
}

// ListRemoteCommits lists the commit and everything it was made from, newest
// first. A commit the provider does not hold is not found.
func (provider *remoteGitTestProvider) ListRemoteCommits(_ context.Context, origin, from string, page int) ([]domain.RemoteCommit, bool, error) {
	if err := provider.request(origin, fmt.Sprintf("commits:%s:%d", strings.TrimLeft(from, "0"), page)); err != nil {
		return nil, false, err
	}
	if _, held := provider.commits[from]; !held {
		return nil, false, outbound.GitProviderStatusError{Status: 404}
	}
	seen, queue, history := map[string]bool{from: true}, []string{from}, []domain.RemoteCommit{}
	for len(queue) > 0 {
		commit := provider.commits[queue[0]]
		queue = queue[1:]
		history = append(history, commit)
		for _, parent := range commit.Parents {
			if _, held := provider.commits[parent]; held && !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	sort.Slice(history, func(left, right int) bool { return history[left].CommittedAt.After(history[right].CommittedAt) })
	listed, more := remoteGitTestPage(history, page, provider.pageSize)
	return listed, more, nil
}

func (provider *remoteGitTestProvider) ListRemotePulls(_ context.Context, origin string, page int) ([]domain.RemotePull, bool, error) {
	if err := provider.request(origin, fmt.Sprintf("pulls:%d", page)); err != nil {
		return nil, false, err
	}
	pulls := []domain.RemotePull{}
	for _, pull := range provider.pulls {
		pull.Commits = []string{}
		pulls = append(pulls, pull)
	}
	sort.Slice(pulls, func(left, right int) bool {
		if !pulls[left].UpdatedAt.Equal(pulls[right].UpdatedAt) {
			return pulls[left].UpdatedAt.After(pulls[right].UpdatedAt)
		}
		return pulls[left].Number > pulls[right].Number
	})
	listed, more := remoteGitTestPage(pulls, page, provider.pageSize)
	return listed, more, nil
}

// ListRemotePullCommits lists a pull request's commits, oldest first, and
// like the real provider no more than 250 of them.
func (provider *remoteGitTestProvider) ListRemotePullCommits(_ context.Context, origin string, number, page int) ([]domain.RemoteCommit, bool, error) {
	if err := provider.request(origin, fmt.Sprintf("pull:%d:%d", number, page)); err != nil {
		return nil, false, err
	}
	commits := []domain.RemoteCommit{}
	for _, sha := range provider.pulls[number].Commits {
		commits = append(commits, provider.commits[sha])
	}
	if len(commits) > domain.RemotePullCommitLimit {
		commits = commits[:domain.RemotePullCommitLimit]
	}
	listed, more := remoteGitTestPage(commits, page, provider.pageSize)
	return listed, more, nil
}

// remoteGitTestStore is the development store with a repository whose origin
// a test can replace. The store itself never replaces an origin once it is
// set; the cache still has to follow one that changed.
type remoteGitTestStore struct {
	*store.FSStore
	origins map[domain.ContentHash]string
	// refusal is what a request for a read fails with, when set.
	refusal error
}

func (storage *remoteGitTestStore) RequestRemoteGitSync(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	if storage.refusal != nil {
		return storage.refusal
	}
	return storage.FSStore.RequestRemoteGitSync(ctx, repo, now)
}

func (storage *remoteGitTestStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	repo, err := storage.FSStore.GetRepo(ctx, id)
	if origin, replaced := storage.origins[id]; replaced && err == nil {
		repo.GitRemoteURL = origin
	}
	return repo, err
}

func (storage *remoteGitTestStore) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	repos, err := storage.FSStore.ListRepos(ctx, team)
	for index := range repos {
		if origin, replaced := storage.origins[repos[index].ID]; replaced {
			repos[index].GitRemoteURL = origin
		}
	}
	return repos, err
}

type remoteGitTestFixture struct {
	service  *Service
	storage  *remoteGitTestStore
	repo     domain.ContentHash
	provider *remoteGitTestProvider
	sync     *RemoteGitSync
	clock    time.Time
}

func newRemoteGitTestFixture(t *testing.T, pageSize int) *remoteGitTestFixture {
	t.Helper()
	storage := &remoteGitTestStore{FSStore: store.NewFSStore(t.TempDir()), origins: map[domain.ContentHash]string{}}
	service := NewService(storage, storage, storage)
	ctx := systemTestContext()
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "test", OwnerID: "owner", Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
	if err := storage.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]domain.MemberRole{"member-a": domain.RoleMember, "viewer": domain.RoleViewer} {
		if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: user, Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main", GitRemoteURL: remoteGitTestOrigin}); err != nil {
		t.Fatal(err)
	}
	fixture := &remoteGitTestFixture{service: service, storage: storage, repo: repo, provider: newRemoteGitTestProvider(pageSize), clock: remoteGitTestTime(100000)}
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: fixture.provider})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return fixture.clock }
	fixture.sync = worker
	return fixture
}

// setOrigin gives the repository another origin, or none.
func (fixture *remoteGitTestFixture) setOrigin(origin string) {
	fixture.storage.origins[fixture.repo] = origin
}

// pass runs one pass and returns the provider requests it made.
func (fixture *remoteGitTestFixture) pass(t *testing.T) ([]string, error) {
	t.Helper()
	fixture.provider.requests = nil
	err := fixture.sync.SyncRemoteGit(context.Background(), fixture.repo)
	return fixture.provider.requests, err
}

func (fixture *remoteGitTestFixture) state(t *testing.T) domain.RemoteGitState {
	t.Helper()
	state, err := fixture.storage.GetRemoteGitState(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture *remoteGitTestFixture) stored(t *testing.T) []domain.RemoteCommit {
	t.Helper()
	commits, err := fixture.storage.ListRemoteCommits(context.Background(), fixture.repo, domain.RemoteCommitCursor{}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return commits
}

func TestRemoteGitPassReadsBranchesCommitsAndPullRequests(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	main := provider.chain(3, "")
	topic := provider.chain(2, main)
	fork := provider.commit(remoteGitTestSHA(1))
	provider.heads["main"], provider.heads["topic"] = main, topic
	provider.pull(1, 50, topic, remoteGitTestSHA(4), topic)
	provider.pull(2, 40, fork, fork)

	start := fixture.clock
	requests, err := fixture.pass(t)
	if err != nil {
		t.Fatal(err)
	}
	// The default branch is walked first, then the others by name; a pull
	// request's commits are read before it is stored.
	want := []string{"default", "heads:1", "commits:3:1", "commits:5:1", "pulls:1", "pull:1:1", "pull:2:1"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests:\n got %v\nwant %v", requests, want)
	}
	state := fixture.state(t)
	if state.DefaultBranch != "main" || state.GitOrigin != remoteGitTestOrigin || state.SyncedAt == nil || !state.SyncedAt.Equal(start) || !state.AttemptedAt.Equal(start) ||
		state.Remaining || !state.PullsComplete || !state.NextAt.Equal(start.Add(domain.RemoteGitSyncEvery)) || len(state.Unreadable) != 0 {
		t.Fatalf("state after the pass: %+v", state)
	}
	ctx := context.Background()
	if branches, err := fixture.storage.ListRemoteBranches(ctx, fixture.repo); err != nil || !reflect.DeepEqual(branches, []domain.RemoteBranch{{Name: "main", SHA: main}, {Name: "topic", SHA: topic}}) {
		t.Fatalf("branches: %+v %v", branches, err)
	}
	commits := fixture.stored(t)
	if len(commits) != 6 || commits[0].SHA != fork || !reflect.DeepEqual(commits[1], provider.commits[topic]) {
		t.Fatalf("commits: %d %+v", len(commits), commits)
	}
	pulls, err := fixture.storage.ListRemotePulls(ctx, fixture.repo)
	if err != nil || len(pulls) != 2 || !reflect.DeepEqual(pulls[0], provider.pulls[2]) || !reflect.DeepEqual(pulls[1], provider.pulls[1]) || pulls[1].Truncated {
		t.Fatalf("pull requests:\n got %+v\nwant %+v %+v\n%v", pulls, provider.pulls[2], provider.pulls[1], err)
	}

	// No pass is due before the next one: the provider is not asked.
	fixture.clock = start.Add(domain.RemoteGitSyncEvery - time.Second)
	if requests, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) || len(requests) != 0 {
		t.Fatalf("a pass before it is due: %v %v", requests, err)
	}
	// A pass over a repository in which nothing changed asks three things and
	// stores the same again.
	fixture.clock = start.Add(domain.RemoteGitSyncEvery)
	requests, err = fixture.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1"}) {
		t.Fatalf("a pass with nothing new: %v %v", requests, err)
	}
	if state := fixture.state(t); !state.SyncedAt.Equal(fixture.clock) || state.Remaining || !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state after the second pass: %+v", state)
	}
	if again := fixture.stored(t); !reflect.DeepEqual(again, commits) {
		t.Fatalf("the second pass changed the commits: %d", len(again))
	}

	// A new commit on a branch, a moved branch and a deleted one.
	second := provider.commit(main)
	provider.heads["main"] = second
	delete(provider.heads, "topic")
	provider.heads["release"] = main
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	requests, err = fixture.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:7:1", "pulls:1"}) {
		t.Fatalf("a pass after a push: %v %v", requests, err)
	}
	if branches, err := fixture.storage.ListRemoteBranches(ctx, fixture.repo); err != nil || !reflect.DeepEqual(branches, []domain.RemoteBranch{{Name: "main", SHA: second}, {Name: "release", SHA: main}}) {
		t.Fatalf("branches after the push: %+v %v", branches, err)
	}
	// The commits of the deleted branch stay.
	if commits := fixture.stored(t); len(commits) != 7 || commits[0].SHA != second {
		t.Fatalf("commits after the push: %d", len(commits))
	}
}

// One pass makes at most forty provider requests. What it could not read is
// read by the passes after it, through the parents that are not stored yet.
func TestRemoteGitPassStaysInsideItsBudget(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 2)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(120, "")
	provider.pull(1, 500, provider.heads["main"], provider.heads["main"])

	start := fixture.clock
	requests, err := fixture.pass(t)
	if err != nil || len(requests) != remoteGitPassBudget {
		t.Fatalf("first pass: %d requests %v", len(requests), err)
	}
	// Two requests found the head; thirty-eight pages of two commits followed.
	if commits := fixture.stored(t); len(commits) != 76 || commits[0].SHA != provider.heads["main"] {
		t.Fatalf("commits after the first pass: %d", len(commits))
	}
	state := fixture.state(t)
	if !state.Remaining || !state.NextAt.Equal(start.Add(time.Minute)) || !state.SyncedAt.Equal(start) || state.PullsComplete || state.PullPage != 1 {
		t.Fatalf("state after a pass that ran out of budget: %+v", state)
	}

	// The head is stored now, so the next pass continues below what is stored.
	fixture.clock = start.Add(time.Minute)
	requests, err = fixture.pass(t)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "heads:1", "pulls:1", "pull:1:1", "commits:2c:1"}; !reflect.DeepEqual(requests[:5], want) {
		t.Fatalf("second pass starts with %v, want %v", requests[:5], want)
	}
	// Forty-four commits are left: twenty-two full pages and the empty one
	// after them.
	if len(requests) != 4+23 {
		t.Fatalf("second pass: %d requests", len(requests))
	}
	if commits := fixture.stored(t); len(commits) != 120 {
		t.Fatalf("commits after the second pass: %d", len(commits))
	}
	state = fixture.state(t)
	if state.Remaining || !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) || !state.PullsComplete {
		t.Fatalf("state after the pass that finished the read: %+v", state)
	}
	if frontier, err := fixture.storage.RemoteCommitFrontier(context.Background(), fixture.repo, 10); err != nil || len(frontier) != 0 {
		t.Fatalf("parents still to read: %v %v", frontier, err)
	}
}

func TestRemoteGitPassCutShortSaysWhatIsLeft(t *testing.T) {
	// The branch list is stored only when every page of it was read.
	fixture := newRemoteGitTestFixture(t, 2)
	provider := fixture.provider
	only := provider.commit()
	for _, name := range []string{"a", "b", "c", "d", "main"} {
		provider.heads[name] = only
	}
	fixture.sync.budget = 3
	requests, err := fixture.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "heads:2"}) {
		t.Fatalf("a pass cut in the branch list: %v %v", requests, err)
	}
	if branches, err := fixture.storage.ListRemoteBranches(context.Background(), fixture.repo); err != nil || len(branches) != 0 {
		t.Fatalf("a branch list that was not read to its end was stored: %+v %v", branches, err)
	}
	if state := fixture.state(t); !state.Remaining || state.SyncedAt == nil || state.PullsComplete {
		t.Fatalf("state: %+v", state)
	}
	// With the budget for it, all three pages are read and stored.
	fixture.sync.budget = remoteGitPassBudget
	fixture.clock = fixture.clock.Add(time.Minute)
	requests, err = fixture.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "heads:2", "heads:3", "commits:1:1", "pulls:1"}) {
		t.Fatalf("the pass after it: %v %v", requests, err)
	}
	if branches, err := fixture.storage.ListRemoteBranches(context.Background(), fixture.repo); err != nil || len(branches) != 5 {
		t.Fatalf("branches: %+v %v", branches, err)
	}
	if state := fixture.state(t); state.Remaining || !state.PullsComplete {
		t.Fatalf("state: %+v", state)
	}

	// A budget that ends exactly with the work leaves nothing.
	exact := newRemoteGitTestFixture(t, 100)
	exact.provider.heads["main"] = exact.provider.chain(3, "")
	exact.sync.budget = 4
	requests, err = exact.pass(t)
	if err != nil || len(requests) != 4 {
		t.Fatalf("a pass with exactly its budget: %v %v", requests, err)
	}
	if state := exact.state(t); state.Remaining || !state.NextAt.Equal(exact.clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state: %+v", state)
	}
	// A budget that ends inside a walk leaves the rest of the walk.
	walk := newRemoteGitTestFixture(t, 2)
	walk.provider.heads["main"] = walk.provider.chain(4, "")
	walk.sync.budget = 4
	requests, err = walk.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:4:1", "commits:4:2"}) {
		t.Fatalf("a pass that ends in a walk: %v %v", requests, err)
	}
	if state := walk.state(t); !state.Remaining || !state.NextAt.Equal(walk.clock.Add(time.Minute)) {
		t.Fatalf("state: %+v", state)
	}
	// A budget that ends with a parent still to read leaves that parent. A
	// pull request lists a commit made on top of one that no branch holds.
	parents := newRemoteGitTestFixture(t, 100)
	root := parents.provider.commit()
	parents.provider.heads["main"] = root
	below := parents.provider.commit(root)
	listed := parents.provider.commit(below)
	parents.provider.pull(1, 50, listed, listed)
	parents.sync.budget = 5
	requests, err = parents.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:1:1", "pulls:1", "pull:1:1"}) {
		t.Fatalf("a pass that ends before the parents: %v %v", requests, err)
	}
	if state := parents.state(t); !state.Remaining || !state.PullsComplete || !state.NextAt.Equal(parents.clock.Add(time.Minute)) {
		t.Fatalf("state: %+v", state)
	}
	parents.clock = parents.clock.Add(time.Minute)
	requests, err = parents.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1", "commits:2:1"}) {
		t.Fatalf("the pass that reads the parent: %v %v", requests, err)
	}
	if state := parents.state(t); state.Remaining || len(parents.stored(t)) != 3 {
		t.Fatalf("state: %+v", state)
	}
}

func TestRemoteGitPassReadsPullRequestsInPages(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 2)
	provider := fixture.provider
	main := provider.commit()
	provider.heads["main"] = main
	for number := 1; number <= 30; number++ {
		head := provider.commit(main)
		provider.pull(number, 1000+number, head, head)
	}

	// The first full read goes through every page. A page costs three
	// requests: the list and two commit lists.
	start := fixture.clock
	requests, err := fixture.pass(t)
	if err != nil || len(requests) != remoteGitPassBudget || requests[3] != "pulls:1" || requests[4] != "pull:30:1" {
		t.Fatalf("first pass: %d requests %v %v", len(requests), requests[:5], err)
	}
	state := fixture.state(t)
	if state.PullsComplete || state.PullPage != 13 || !state.Remaining || !state.NextAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("state after the first pass: %+v", state)
	}
	pulls, err := fixture.storage.ListRemotePulls(context.Background(), fixture.repo)
	if err != nil || len(pulls) != 24 {
		t.Fatalf("pull requests after the first pass: %d %v", len(pulls), err)
	}
	// The next pass continues on the page the first one was on.
	fixture.clock = start.Add(time.Minute)
	requests, err = fixture.pass(t)
	if err != nil || requests[2] != "pulls:13" || requests[len(requests)-1] != "pulls:16" {
		t.Fatalf("second pass: %v %v", requests, err)
	}
	state = fixture.state(t)
	if !state.PullsComplete || state.PullPage != 0 || state.Remaining {
		t.Fatalf("state after the second pass: %+v", state)
	}
	if pulls, err = fixture.storage.ListRemotePulls(context.Background(), fixture.repo); err != nil || len(pulls) != 30 || pulls[0].Number != 30 {
		t.Fatalf("pull requests after the second pass: %d %v", len(pulls), err)
	}

	// After the first full read a pass stops at the first page on which
	// nothing changed.
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if requests, err = fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1"}) {
		t.Fatalf("a pass with nothing new: %v %v", requests, err)
	}
	// A pull request whose branch was pushed anew is the most recently
	// updated one.
	moved := provider.pulls[7]
	added := provider.commit(main)
	moved.HeadSHA, moved.Commits, moved.UpdatedAt, moved.State = added, []string{added}, remoteGitTestTime(5000), domain.RemotePullClosed
	provider.pulls[7] = moved
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if requests, err = fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1", "pull:7:1", "pulls:2"}) {
		t.Fatalf("a pass after a pull request changed: %v %v", requests, err)
	}
	pulls, err = fixture.storage.ListRemotePulls(context.Background(), fixture.repo)
	if err != nil || len(pulls) != 30 || !reflect.DeepEqual(pulls[23], moved) {
		t.Fatalf("the changed pull request:\n got %+v\nwant %+v\n%v", pulls[23], moved, err)
	}
	// A new title alone is a change too: the update time moved.
	renamed := provider.pulls[3]
	renamed.Title, renamed.UpdatedAt = "Another title", remoteGitTestTime(6000)
	provider.pulls[3] = renamed
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if requests, err = fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1", "pull:3:1", "pulls:2"}) {
		t.Fatalf("a pass after a pull request was renamed: %v %v", requests, err)
	}
}

func TestRemoteGitPassCutAmongChangedPullRequestsReadsTheRestLater(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 2)
	provider := fixture.provider
	main := provider.commit()
	provider.heads["main"] = main
	for number := 1; number <= 30; number++ {
		head := provider.commit(main)
		provider.pull(number, 1000+number, head, head)
	}
	// Two passes make the first full read.
	for pass := 0; pass < 2; pass++ {
		if _, err := fixture.pass(t); err != nil {
			t.Fatal(err)
		}
		fixture.clock = fixture.clock.Add(time.Minute)
	}
	if state := fixture.state(t); !state.PullsComplete {
		t.Fatalf("state after the first full read: %+v", state)
	}
	// Every pull request changes at once: more than one pass can read.
	moved := map[int]string{}
	for number := 1; number <= 30; number++ {
		pull := provider.pulls[number]
		pull.HeadSHA = provider.commit(main)
		pull.Commits, pull.UpdatedAt = []string{pull.HeadSHA}, remoteGitTestTime(9000+number)
		provider.pulls[number], moved[number] = pull, pull.HeadSHA
	}
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	requests, err := fixture.pass(t)
	if err != nil || len(requests) != remoteGitPassBudget {
		t.Fatalf("the pass that is cut: %d requests %v", len(requests), err)
	}
	// It was cut on page 13, and says the next passes read on from there.
	state := fixture.state(t)
	if state.PullsComplete || state.PullPage != 13 || !state.Remaining {
		t.Fatalf("state after the cut pass: %+v", state)
	}
	fixture.clock = fixture.clock.Add(time.Minute)
	requests, err = fixture.pass(t)
	if err != nil || requests[2] != "pulls:13" {
		t.Fatalf("the next pass: %v %v", requests, err)
	}
	if state = fixture.state(t); !state.PullsComplete || state.PullPage != 0 {
		t.Fatalf("state after the rest was read: %+v", state)
	}
	pulls, err := fixture.storage.ListRemotePulls(context.Background(), fixture.repo)
	if err != nil || len(pulls) != 30 {
		t.Fatalf("pull requests: %d %v", len(pulls), err)
	}
	for _, pull := range pulls {
		if pull.HeadSHA != moved[pull.Number] {
			t.Errorf("pull request %d was left as it was before it changed", pull.Number)
		}
	}
	// A pass with no budget left for pull requests at all learns nothing
	// about them and does not start a full read.
	fixture.sync.budget = 2
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if state = fixture.state(t); !state.PullsComplete {
		t.Fatalf("a pass that never reached the pull requests: %+v", state)
	}
}

func TestRemoteGitPassKeepsWhatThePullRequestListHolds(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	main := provider.commit()
	provider.heads["main"] = main
	long := []string{}
	for parent, index := main, 0; index < 260; index++ {
		parent = provider.commit(parent)
		long = append(long, parent)
	}
	provider.pull(1, 500, long[len(long)-1], long...)
	exact := []string{}
	for parent, index := main, 0; index < 250; index++ {
		parent = provider.commit(parent)
		exact = append(exact, parent)
	}
	provider.pull(2, 400, exact[len(exact)-1], exact...)
	provider.pull(3, 300, long[2], long[:3]...)
	provider.pull(4, 200, main)

	requests, err := fixture.pass(t)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "heads:1", "commits:1:1", "pulls:1", "pull:1:1", "pull:1:2", "pull:1:3", "pull:2:1", "pull:2:2", "pull:2:3", "pull:3:1", "pull:4:1"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests:\n got %v\nwant %v", requests, want)
	}
	pulls, err := fixture.storage.ListRemotePulls(context.Background(), fixture.repo)
	if err != nil || len(pulls) != 4 {
		t.Fatalf("pull requests: %d %v", len(pulls), err)
	}
	// The provider's list stops at 250 commits: a list of that length may be
	// cut short.
	for _, check := range []struct {
		pull      domain.RemotePull
		commits   []string
		truncated bool
	}{{pulls[3], long[:250], true}, {pulls[2], exact, true}, {pulls[1], long[:3], false}, {pulls[0], []string{}, false}} {
		if !reflect.DeepEqual(check.pull.Commits, check.commits) || check.pull.Truncated != check.truncated {
			t.Errorf("pull request %d: %d commits, truncated=%v", check.pull.Number, len(check.pull.Commits), check.pull.Truncated)
		}
	}
	// The commits a pull request lists are stored with it.
	if missing, err := fixture.storage.MissingRemoteCommits(context.Background(), fixture.repo, append(append([]string{}, long[:250]...), exact...)); err != nil || len(missing) != 0 {
		t.Fatalf("listed commits that are not stored: %d %v", len(missing), err)
	}
	if missing, err := fixture.storage.MissingRemoteCommits(context.Background(), fixture.repo, long[250:]); err != nil || len(missing) != 10 {
		t.Fatalf("commits beyond the provider's list: %d %v", len(missing), err)
	}
}

func TestRemoteGitPassRemembersCommitsTheProviderRefuses(t *testing.T) {
	for _, status := range []int{404, 409, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fixture := newRemoteGitTestFixture(t, 100)
			provider := fixture.provider
			// The provider holds a history whose first commit names a parent
			// it does not hold, and another parent it refuses to list.
			gone, refused := remoteGitTestSHA(0x900), remoteGitTestSHA(0x901)
			first := provider.commit(gone, refused)
			provider.heads["main"] = provider.commit(first)
			provider.failures["commits:901:1"] = outbound.GitProviderStatusError{Status: status}
			if status != 404 {
				provider.failures["commits:900:1"] = outbound.GitProviderStatusError{Status: status}
			}

			requests, err := fixture.pass(t)
			if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:2:1", "pulls:1", "commits:900:1", "commits:901:1"}) {
				t.Fatalf("first pass: %v %v", requests, err)
			}
			state := fixture.state(t)
			if !reflect.DeepEqual(state.Unreadable, []string{gone, refused}) || state.Remaining || state.SyncedAt == nil || !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
				t.Fatalf("state after the provider refused two commits: %+v", state)
			}
			// They are not asked for again.
			fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
			if requests, err = fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1"}) {
				t.Fatalf("second pass: %v %v", requests, err)
			}
			if state := fixture.state(t); !reflect.DeepEqual(state.Unreadable, []string{gone, refused}) {
				t.Fatalf("state after the second pass: %+v", state)
			}
		})
	}

	// Any other answer is a failure of the pass, and the commit stays to be read.
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.commit(remoteGitTestSHA(0x900))
	provider.failures["commits:900:1"] = outbound.GitProviderStatusError{Status: 502}
	if _, err := fixture.pass(t); err == nil {
		t.Fatal("a provider failure was taken for a refusal")
	}
	if state := fixture.state(t); len(state.Unreadable) != 0 || state.SyncedAt != nil {
		t.Fatalf("state after a provider failure: %+v", state)
	}
	// A branch head the provider does not list is a failure as well: nothing
	// says the head is gone for good.
	provider.heads["main"] = remoteGitTestSHA(0x902)
	fixture.clock = fixture.clock.Add(2 * time.Minute)
	if _, err := fixture.pass(t); !errors.Is(err, errRemoteGitUnreadable) {
		t.Fatalf("a head the provider does not list: %v", err)
	}
	if state := fixture.state(t); len(state.Unreadable) != 0 || state.SyncedAt != nil {
		t.Fatalf("state after an unlisted head: %+v", state)
	}
}

func TestRemoteGitPassThatFailsKeepsWhatItStored(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(3, "")
	provider.pull(1, 50, provider.heads["main"], provider.heads["main"])

	// A rate limit is a failure like any other.
	provider.failures["pulls:1"] = outbound.ErrGitProviderRateLimited
	start := fixture.clock
	requests, err := fixture.pass(t)
	if !errors.Is(err, outbound.ErrGitProviderRateLimited) || len(requests) != 4 {
		t.Fatalf("a pass that failed: %v %v", requests, err)
	}
	state := fixture.state(t)
	if state.SyncedAt != nil || !state.NextAt.Equal(start.Add(2*time.Minute)) || !state.AttemptedAt.Equal(start) || state.DefaultBranch != "main" || state.PullsComplete {
		t.Fatalf("state after the failure: %+v", state)
	}
	if commits := fixture.stored(t); len(commits) != 3 {
		t.Fatalf("commits after the failure: %d", len(commits))
	}
	if branches, err := fixture.storage.ListRemoteBranches(context.Background(), fixture.repo); err != nil || len(branches) != 1 {
		t.Fatalf("branches after the failure: %+v %v", branches, err)
	}
	// The claim was released: the pass after the retry time runs.
	fixture.clock = start.Add(2*time.Minute - time.Second)
	if _, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a pass before the retry time: %v", err)
	}
	delete(provider.failures, "pulls:1")
	fixture.clock = start.Add(2 * time.Minute)
	if requests, err = fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "pulls:1", "pull:1:1"}) {
		t.Fatalf("the pass after the failure: %v %v", requests, err)
	}
	if state := fixture.state(t); !state.SyncedAt.Equal(fixture.clock) || !state.PullsComplete {
		t.Fatalf("state after the retry: %+v", state)
	}
	// A failure after a read keeps the time of that read.
	read := fixture.clock
	provider.failures["default"] = errors.New("https://provider.invalid/secret is unreachable")
	fixture.clock = read.Add(domain.RemoteGitSyncEvery)
	if requests, err = fixture.pass(t); err == nil || len(requests) != 1 {
		t.Fatalf("a pass that failed at once: %v %v", requests, err)
	}
	if state := fixture.state(t); !state.SyncedAt.Equal(read) || !state.NextAt.Equal(fixture.clock.Add(2*time.Minute)) {
		t.Fatalf("state after a failure that follows a read: %+v", state)
	}
}

func TestRemoteGitCacheBelongsToOneOrigin(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(3, "")
	provider.pull(1, 50, provider.heads["main"], provider.heads["main"])
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	member := anchoredActor("member-a")
	page, err := fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, 10)
	if err != nil || len(page.Commits) != 3 || len(page.Branches) != 1 || len(page.Pulls) != 1 || page.DefaultBranch != "main" || page.SyncedAt == nil {
		t.Fatalf("what a member reads: %+v %v", page, err)
	}

	// The repository gets another origin. What was read from the first one
	// is not shown as the repository's any more, before any pass ran.
	const moved = "https://github.com/example/moved"
	fixture.setOrigin(moved)
	page, err = fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, 10)
	if err != nil || len(page.Commits) != 0 || len(page.Branches) != 0 || len(page.Pulls) != 0 || page.DefaultBranch != "" || page.SyncedAt != nil || page.Next != nil {
		t.Fatalf("what a member reads after the origin changed: %+v %v", page, err)
	}
	// The next pass does not wait: it removes the cache and reads the new origin.
	provider.heads, provider.commits, provider.pulls, provider.next = map[string]string{}, map[string]domain.RemoteCommit{}, map[int]domain.RemotePull{}, 500
	provider.defaultBranch = "trunk"
	provider.heads["trunk"] = provider.chain(2, "")
	fixture.clock = fixture.clock.Add(time.Second)
	if requests, err := fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:1f6:1", "pulls:1"}) {
		t.Fatalf("the first pass over the new origin: %v %v", requests, err)
	}
	if provider.origins[moved] != 4 {
		t.Fatalf("requests by origin: %v", provider.origins)
	}
	state := fixture.state(t)
	if state.GitOrigin != moved || state.DefaultBranch != "trunk" || !state.SyncedAt.Equal(fixture.clock) {
		t.Fatalf("state of the new origin: %+v", state)
	}
	if commits := fixture.stored(t); len(commits) != 2 || commits[0].SHA != provider.heads["trunk"] {
		t.Fatalf("commits of the new origin: %d", len(commits))
	}
	if pulls, err := fixture.storage.ListRemotePulls(context.Background(), fixture.repo); err != nil || len(pulls) != 0 {
		t.Fatalf("pull requests of the first origin were kept: %+v %v", pulls, err)
	}
	// A repository without an origin is not read.
	fixture.setOrigin("")
	fixture.clock = fixture.clock.Add(time.Hour)
	if requests, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) || len(requests) != 0 {
		t.Fatalf("a pass over a repository without an origin: %v %v", requests, err)
	}
}

// What a pass stores, it stores for the origin it read. When the origin
// changes while the pass runs, the rest of the pass stores nothing.
func TestRemoteGitPassStopsStoringWhenTheOriginChanges(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(3, "")
	provider.before = func(request string) {
		if request == "commits:3:1" {
			fixture.setOrigin("https://github.com/example/moved")
		}
	}
	requests, err := fixture.pass(t)
	if !errors.Is(err, domain.ErrConflict) || len(requests) != 3 {
		t.Fatalf("a pass whose origin changed: %v %v", requests, err)
	}
	if commits := fixture.stored(t); len(commits) != 0 {
		t.Fatalf("commits of the old origin stored after the change: %d", len(commits))
	}
}

func TestRemoteGitReadAskedForDuringAPassIsNotLost(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(2, "")
	start := fixture.clock
	// A push arrives after the pass read the branches.
	provider.before = func(request string) {
		if request == "pulls:1" {
			if err := fixture.storage.RequestRemoteGitSync(context.Background(), fixture.repo, start.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	provider.before = nil
	state := fixture.state(t)
	if state.RequestedAt == nil || !state.RequestedAt.Equal(start.Add(4*time.Second)) || !state.NextAt.Equal(start.Add(domain.RemoteGitRequestSpacing)) || !state.SyncedAt.Equal(start) {
		t.Fatalf("state after a request during the pass: %+v", state)
	}
	fixture.clock = start.Add(domain.RemoteGitRequestSpacing - time.Second)
	if _, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a pass closer to the last one than the spacing: %v", err)
	}
	fixture.clock = start.Add(domain.RemoteGitRequestSpacing)
	if _, err := fixture.pass(t); err != nil {
		t.Fatalf("the pass that was asked for: %v", err)
	}
	if state := fixture.state(t); !state.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state after the pass that was asked for: %+v", state)
	}
}

// A pass whose claim another pass took over stores nothing when it ends.
func TestRemoteGitPassWithoutItsClaimDoesNotFinish(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(2, "")
	ctx := context.Background()
	later := fixture.clock.Add(3 * time.Minute)
	provider.before = func(request string) {
		if request != "pulls:1" {
			return
		}
		// The lease of the running pass ended and another worker claimed.
		if _, err := fixture.storage.ClaimRemoteGitSync(ctx, fixture.repo, remoteGitTestOrigin, later, remoteGitLease); err != nil {
			t.Fatalf("the other worker's claim: %v", err)
		}
	}
	if _, err := fixture.pass(t); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a pass that lost its claim: %v", err)
	}
	state := fixture.state(t)
	if !state.AttemptedAt.Equal(later) || state.SyncedAt != nil || state.PullsComplete {
		t.Fatalf("the other pass's state was replaced: %+v", state)
	}
	// The other worker still holds its claim.
	if _, err := fixture.storage.ClaimRemoteGitSync(ctx, fixture.repo, remoteGitTestOrigin, later.Add(time.Second), remoteGitLease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the other worker's claim was released: %v", err)
	}
	// While another worker holds the claim, no pass starts.
	provider.before = nil
	fixture.clock = later.Add(time.Second)
	if requests, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) || len(requests) != 0 {
		t.Fatalf("a pass while another worker holds the claim: %v %v", requests, err)
	}
}

func TestRemoteGitWorkerReadsRepositoriesThatAreDue(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(2, "")
	ctx := systemTestContext()
	// A repository without an origin, and one whose origin was read a moment ago.
	without := domain.Repo{ID: hh(t.Name() + "/without"), DefaultBranch: "main"}
	recent := domain.Repo{ID: hh(t.Name() + "/recent"), DefaultBranch: "main", GitRemoteURL: "https://github.com/example/recent"}
	for _, repo := range []domain.Repo{without, recent} {
		if _, err := fixture.storage.PutRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := fixture.storage.ClaimRemoteGitSync(ctx, recent.ID, recent.GitRemoteURL, fixture.clock.Add(-time.Minute), remoteGitLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.FinishRemoteGitSync(ctx, claimed.Ended(fixture.clock.Add(-time.Minute), false, false)); err != nil {
		t.Fatal(err)
	}

	if err := fixture.sync.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(provider.origins, map[string]int{remoteGitTestOrigin: 4}) {
		t.Fatalf("requests by origin: %v", provider.origins)
	}
	if _, err := fixture.storage.GetRemoteGitState(ctx, without.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a repository without an origin got a state: %v", err)
	}
	// Nothing is due right after: the provider is not asked.
	if err := fixture.sync.Process(context.Background()); err != nil || provider.origins[remoteGitTestOrigin] != 4 {
		t.Fatalf("a second round at once: %v %v", provider.origins, err)
	}
	// A repository that fails does not stop the ones after it.
	provider.failures["default"] = outbound.GitProviderStatusError{Status: 503}
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if err := fixture.sync.Process(context.Background()); err != nil || provider.origins[recent.GitRemoteURL] != 1 || provider.origins[remoteGitTestOrigin] != 5 {
		t.Fatalf("a round in which every read fails: %v %v", provider.origins, err)
	}
	// A cancelled round stops.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fixture.sync.Process(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled round: %v", err)
	}
	fixture.sync.Run(cancelled)
}

func TestRemoteGitSyncNeedsItsStoreAndProvider(t *testing.T) {
	storage := store.NewFSStore(t.TempDir())
	provider := newRemoteGitTestProvider(100)
	var absent *remoteGitTestProvider
	for name, config := range map[string]RemoteGitSyncConfig{
		"no storage":           {Reader: provider},
		"no provider":          {Metadata: storage, Blobs: storage, Repositories: storage},
		"an absent provider":   {Metadata: storage, Blobs: storage, Repositories: storage, Reader: absent},
		"storage without it":   {Metadata: struct{ outbound.MetadataStore }{storage}, Reader: provider},
		"an absent blob store": {Metadata: storage, Blobs: (*store.FSStore)(nil), Repositories: storage, Reader: provider},
	} {
		if worker, err := NewRemoteGitSync(config); !errors.Is(err, domain.ErrValidation) || worker != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRemoteGitReadAndRefresh(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(5, "")
	provider.pull(1, 50, provider.heads["main"], provider.heads["main"])
	member, viewer := anchoredActor("member-a"), anchoredActor("viewer")

	// A repository never read reads as empty lists.
	page, err := fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, 10)
	if err != nil || page.Commits == nil || page.Branches == nil || page.Pulls == nil || len(page.Commits)+len(page.Branches)+len(page.Pulls) != 0 || page.SyncedAt != nil || page.Next != nil {
		t.Fatalf("before any read: %#v %v", page, err)
	}
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	page, err = fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, 2)
	if err != nil || len(page.Commits) != 2 || page.Commits[0].SHA != remoteGitTestSHA(5) || page.Next == nil || *page.Next != domain.RemoteCommitCursorAt(page.Commits[1]) ||
		page.DefaultBranch != "main" || !page.SyncedAt.Equal(fixture.clock) || len(page.Branches) != 1 || len(page.Pulls) != 1 {
		t.Fatalf("the first page: %+v %v", page, err)
	}
	// A page further down carries commits only.
	page, err = fixture.service.ReadRemoteGit(member, fixture.repo, *page.Next, 2)
	if err != nil || len(page.Commits) != 2 || page.Commits[0].SHA != remoteGitTestSHA(3) || page.Next == nil ||
		page.DefaultBranch != "" || page.SyncedAt != nil || len(page.Branches) != 0 || len(page.Pulls) != 0 {
		t.Fatalf("the second page: %+v %v", page, err)
	}
	page, err = fixture.service.ReadRemoteGit(member, fixture.repo, *page.Next, 2)
	if err != nil || len(page.Commits) != 1 || page.Next != nil {
		t.Fatalf("the last page: %+v %v", page, err)
	}
	// A page that holds exactly what is left is the last one.
	if page, err = fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, 5); err != nil || len(page.Commits) != 5 || page.Next != nil {
		t.Fatalf("a page of everything: %+v %v", page, err)
	}
	for _, limit := range []int{0, -1, domain.RemoteGitPageLimit + 1} {
		if _, err := fixture.service.ReadRemoteGit(member, fixture.repo, domain.RemoteCommitCursor{}, limit); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("a page of %d commits: %v", limit, err)
		}
	}
	if _, err := fixture.service.ReadRemoteGit(member, "not-a-repository", domain.RemoteCommitCursor{}, 10); err == nil {
		t.Fatal("a read of what is not a repository")
	}

	// Asking for a fresh read pulls the next pass closer.
	if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); err != nil {
		t.Fatal(err)
	}
	state := fixture.state(t)
	if state.RequestedAt == nil || !state.NextAt.Before(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state after a member asked: %+v", state)
	}
	if err := fixture.service.RequestRemoteGitRefresh(viewer, fixture.repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer asking: %v", err)
	}
	if err := fixture.service.RequestRemoteGitRefresh(anchoredActor("stranger"), fixture.repo); !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an account outside the repository asking: %v", err)
	}
	// A repository without an origin has nothing to read.
	fixture.setOrigin("")
	if err := fixture.service.RequestRemoteGitRefresh(member, fixture.repo); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("asking for a repository without an origin: %v", err)
	}
	// Asking for a repository that was never read is accepted: its first
	// pass is due anyway.
	never := newRemoteGitTestFixture(t, 100)
	if err := never.service.RequestRemoteGitRefresh(member, never.repo); err != nil {
		t.Fatalf("asking before any read: %v", err)
	}
	if _, err := never.storage.GetRemoteGitState(context.Background(), never.repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("asking made a state: %v", err)
	}
}

// A push the hosting provider delivers asks for a fresh read in the
// transaction that stores its observation.
func TestRemoteGitReadIsAskedForByPushObservations(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	fixture.provider.heads["main"] = fixture.provider.chain(2, "")
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if state := fixture.state(t); state.RequestedAt != nil {
		t.Fatalf("state before the push: %+v", state)
	}
	if _, err := fixture.service.ObserveRemotePush(systemTestContext(), remoteGitTestOrigin, "refs/heads/main", "", strings.Repeat("1", 40), false, "delivery-1"); err != nil {
		t.Fatal(err)
	}
	state := fixture.state(t)
	if state.RequestedAt == nil || !state.NextAt.Before(fixture.clock.Add(domain.RemoteGitSyncEvery)) {
		t.Fatalf("state after the push: %+v", state)
	}
	// A store without the cache is asked for nothing.
	if err := requestRemoteGitSync(context.Background(), struct{}{}, fixture.repo, time.Now()); err != nil {
		t.Fatalf("a store without the cache: %v", err)
	}
}

func TestRemoteGitReadIsAskedForByQualifiedPushDeliveries(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Hour)
	claimed, err := storage.ClaimRemoteGitSync(ctx, repo.ID, repo.GitRemoteURL, started, remoteGitLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishRemoteGitSync(ctx, claimed.Ended(started, false, false).Ended(time.Now().UTC(), false, false)); err != nil {
		t.Fatal(err)
	}
	before, err := storage.GetRemoteGitState(ctx, repo.ID)
	if err != nil || before.RequestedAt != nil || !before.NextAt.After(time.Now().Add(4*time.Minute)) {
		t.Fatalf("state before the push: %+v %v", before, err)
	}
	body := []byte(`{"installation":{"id":789},"repository":{"id":91},"ref":"refs/heads/topic","before":"` + strings.Repeat("0", 40) + `","after":"` + strings.Repeat("c", 40) + `"}`)
	if err := worker.Receive(ctx, "remote-git-push", "push", body); err != nil {
		t.Fatal(err)
	}
	if err := githubWorkerOf(worker).deliver(ctx, githubHash("remote-git-push")); err != nil {
		t.Fatal(err)
	}
	after, err := storage.GetRemoteGitState(ctx, repo.ID)
	if err != nil || after.RequestedAt == nil || !after.NextAt.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("state after the push: %+v %v", after, err)
	}
}

// A push from a computer that tells the server nothing is read when somebody
// looks: a look at the page from the top asks for a read once the last pass
// started five minutes ago or more. The service goes by the time it is, so
// the passes of this test start minutes before now.
func TestRemoteGitLookAsksForARead(t *testing.T) {
	member, viewer := anchoredActor("member-a"), anchoredActor("viewer")
	ago := func(d time.Duration) time.Time { return time.Now().UTC().Add(-d).Truncate(time.Second) }
	top := domain.RemoteCommitCursor{}
	look := func(t *testing.T, fixture *remoteGitTestFixture, actor context.Context, before domain.RemoteCommitCursor) domain.RemoteGitPage {
		t.Helper()
		page, err := fixture.service.ReadRemoteGit(actor, fixture.repo, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	readAt := func(t *testing.T, started time.Time) *remoteGitTestFixture {
		t.Helper()
		fixture := newRemoteGitTestFixture(t, 100)
		fixture.provider.heads["main"] = fixture.provider.chain(3, "")
		fixture.clock = started
		if _, err := fixture.pass(t); err != nil {
			t.Fatal(err)
		}
		return fixture
	}

	t.Run("an old read", func(t *testing.T) {
		fixture := readAt(t, ago(10*time.Minute))
		read := fixture.state(t)
		// A page further down is no look at what was read.
		first := look(t, fixture, member, top)
		if state := fixture.state(t); first.Next == nil || state.RequestedAt == nil {
			t.Fatalf("the first look: %+v %+v", first, state)
		}
		// That first look asked. What it asked is undone here, to see the
		// page further down ask nothing by itself.
		if err := fixture.storage.FinishRemoteGitSync(context.Background(), read); err != nil {
			t.Fatal(err)
		}
		look(t, fixture, member, *first.Next)
		if state := fixture.state(t); !reflect.DeepEqual(state, read) {
			t.Fatalf("a page further down changed the state:\n got %+v\nwant %+v", state, read)
		}

		// A viewer may read the page, and so asks like anyone.
		before := time.Now().UTC()
		page := look(t, fixture, viewer, top)
		after := time.Now().UTC()
		state := fixture.state(t)
		if state.RequestedAt == nil || state.RequestedAt.Before(before) || state.RequestedAt.After(after) || !state.NextAt.Equal(*state.RequestedAt) {
			t.Fatalf("state after a viewer looked: %+v, between %s and %s", state, before, after)
		}
		// The reader is given what the server holds now.
		if !page.SyncedAt.Equal(fixture.clock) || len(page.Commits) != 2 || page.Commits[0].SHA != remoteGitTestSHA(3) || len(page.Branches) != 1 {
			t.Fatalf("the page of the look that asked: %+v", page)
		}
		// A pass is due now: another look asks nothing more.
		look(t, fixture, member, top)
		if again := fixture.state(t); !reflect.DeepEqual(again, state) {
			t.Fatalf("a second look changed the state:\n got %+v\nwant %+v", again, state)
		}
		// The pass that was asked for reads what was pushed meanwhile, and
		// the look after it shows that and asks nothing.
		fixture.provider.heads["main"] = fixture.provider.chain(2, fixture.provider.heads["main"])
		fixture.clock = time.Now().UTC().Truncate(time.Second).Add(time.Second)
		if requests, err := fixture.pass(t); err != nil || len(requests) == 0 {
			t.Fatalf("the pass a look asked for: %v %v", requests, err)
		}
		finished := fixture.state(t)
		page = look(t, fixture, viewer, top)
		if !page.SyncedAt.Equal(fixture.clock) || page.Commits[0].SHA != remoteGitTestSHA(5) {
			t.Fatalf("the page after that pass: %+v", page)
		}
		if state := fixture.state(t); !reflect.DeepEqual(state, finished) {
			t.Fatalf("a look right after a pass changed the state:\n got %+v\nwant %+v", state, finished)
		}
	})

	t.Run("a recent read", func(t *testing.T) {
		fixture := readAt(t, ago(time.Minute))
		read := fixture.state(t)
		if page := look(t, fixture, member, top); !page.SyncedAt.Equal(fixture.clock) {
			t.Fatalf("the page: %+v", page)
		}
		if state := fixture.state(t); !reflect.DeepEqual(state, read) || state.RequestedAt != nil {
			t.Fatalf("a look a minute after a pass changed the state:\n got %+v\nwant %+v", state, read)
		}
	})

	t.Run("a pass that just failed", func(t *testing.T) {
		// The last read is twenty minutes old, and a pass that started a
		// minute ago failed: it is tried again by its own rule, and a look
		// does not hurry it.
		fixture := readAt(t, ago(20*time.Minute))
		// A request two minutes ago made the pass of a minute ago due.
		if err := fixture.storage.RequestRemoteGitSync(context.Background(), fixture.repo, ago(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		unreachable := errors.New("the provider is unreachable")
		fixture.provider.failures["default"] = unreachable
		fixture.clock = ago(time.Minute)
		if _, err := fixture.pass(t); !errors.Is(err, unreachable) {
			t.Fatalf("the pass did not fail with the provider's failure: %v", err)
		}
		failed := fixture.state(t)
		if !failed.AttemptedAt.Equal(fixture.clock) || !failed.SyncedAt.Before(fixture.clock) || !failed.NextAt.Equal(fixture.clock.Add(domain.RemoteGitSyncRetry)) {
			t.Fatalf("state after the failure: %+v", failed)
		}
		look(t, fixture, member, top)
		if state := fixture.state(t); !reflect.DeepEqual(state, failed) {
			t.Fatalf("a look after a failed pass changed the state:\n got %+v\nwant %+v", state, failed)
		}
	})

	t.Run("nothing to ask about", func(t *testing.T) {
		// A repository never read has no state, and a look makes none.
		never := newRemoteGitTestFixture(t, 100)
		look(t, never, member, top)
		if _, err := never.storage.GetRemoteGitState(context.Background(), never.repo); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("a look at a repository never read made a state: %v", err)
		}
		// What was read from an origin the repository no longer has is not
		// shown, and a look at it asks nothing.
		moved := readAt(t, ago(10*time.Minute))
		read := moved.state(t)
		moved.setOrigin("https://github.com/example/elsewhere")
		if page := look(t, moved, member, top); page.SyncedAt != nil || len(page.Commits) != 0 {
			t.Fatalf("the page of another origin: %+v", page)
		}
		if state := moved.state(t); !reflect.DeepEqual(state, read) {
			t.Fatalf("a look at another origin changed the state:\n got %+v\nwant %+v", state, read)
		}
	})

	t.Run("a request that fails", func(t *testing.T) {
		// The reader still gets the page.
		fixture := readAt(t, ago(10*time.Minute))
		read := fixture.state(t)
		fixture.storage.refusal = errors.New("the store is unavailable")
		if page := look(t, fixture, member, top); !page.SyncedAt.Equal(fixture.clock) || len(page.Commits) != 2 {
			t.Fatalf("the page when the request failed: %+v", page)
		}
		if state := fixture.state(t); !reflect.DeepEqual(state, read) {
			t.Fatalf("a request that failed changed the state:\n got %+v\nwant %+v", state, read)
		}
	})
}

// A walk from a branch head stops at the first page that reaches commits the
// store holds: the page after it would repeat them. A push of a few commits
// onto stored history costs one request for its commits, not two.
func TestRemoteGitWalkStopsWhereStoredHistoryBegins(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 3)
	provider := fixture.provider
	provider.heads["main"] = provider.chain(4, "")
	// Four commits, three to a page: two pages, and the second is the last.
	if requests, err := fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:4:1", "commits:4:2", "pulls:1"}) {
		t.Fatalf("the first read: %v %v", requests, err)
	}
	// Two commits are pushed on top. Their page also holds a commit that is
	// stored, and nothing more is asked about that head.
	provider.heads["main"] = provider.chain(2, provider.heads["main"])
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if requests, err := fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:6:1", "pulls:1"}) {
		t.Fatalf("the read after a push of two commits: %v %v", requests, err)
	}
	if commits := fixture.stored(t); len(commits) != 6 {
		t.Fatalf("%d commits are stored, the provider holds 6", len(commits))
	}
	// A page of new commits only is followed by the next one.
	provider.heads["main"] = provider.chain(4, provider.heads["main"])
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	if requests, err := fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:a:1", "commits:a:2", "pulls:1"}) {
		t.Fatalf("the read after a push of four commits: %v %v", requests, err)
	}
	if commits := fixture.stored(t); len(commits) != 10 {
		t.Fatalf("%d commits are stored, the provider holds 10", len(commits))
	}
}

// What lies behind the page a walk stopped at is not lost. The provider lists
// history newest first, so the side of a merge that is older than the stored
// commits comes pages later. A stored commit names it as its parent, and the
// frontier reads from there.
func TestRemoteGitMergeOfAnOlderSideIsReadThroughTheFrontier(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 3)
	provider := fixture.provider
	side := provider.chain(3, "")
	provider.heads["main"] = provider.chain(4, "")
	if requests, err := fixture.pass(t); err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:7:1", "commits:7:2", "pulls:1"}) {
		t.Fatalf("the first read: %v %v", requests, err)
	}
	if commits := fixture.stored(t); len(commits) != 4 {
		t.Fatalf("%d commits are stored before the merge, the branch holds 4", len(commits))
	}
	// The merge is the newest commit; the side it brings in is the oldest.
	provider.heads["main"] = provider.commit(provider.heads["main"], side)
	fixture.clock = fixture.clock.Add(domain.RemoteGitSyncEvery)
	requests, err := fixture.pass(t)
	if err != nil || !reflect.DeepEqual(requests, []string{"default", "heads:1", "commits:8:1", "pulls:1", "commits:3:1", "commits:3:2"}) {
		t.Fatalf("the read after the merge: %v %v", requests, err)
	}
	stored := map[string]bool{}
	for _, commit := range fixture.stored(t) {
		stored[commit.SHA] = true
	}
	for number := 1; number <= 8; number++ {
		if !stored[remoteGitTestSHA(number)] {
			t.Fatalf("commit %d is not stored after the merge was read: %v", number, stored)
		}
	}
	if state := fixture.state(t); state.Remaining {
		t.Fatalf("the pass says something is left to read: %+v", state)
	}
}

// A commit is stored with the push it came with: the newest commit of the
// push the server was reading when it first stored the commit. The commits
// of the first read came with no push the server saw.
func TestRemoteGitCommitsAreStoredWithThePushTheyCameWith(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	provider := fixture.provider
	old := provider.chain(3, "")
	provider.heads["main"] = old
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	heads := func() map[string]string {
		t.Helper()
		with := map[string]string{}
		for _, commit := range fixture.stored(t) {
			with[commit.SHA] = commit.Head
		}
		return with
	}
	for sha, head := range heads() {
		if head != "" {
			t.Fatalf("commit %s of the first read came with the push %s", sha, head)
		}
	}
	// Two commits are pushed to main, and a new branch with one commit of
	// its own on top of the old history.
	first := provider.commit(old)
	pushed := provider.commit(first)
	provider.heads["main"] = pushed
	topic := provider.commit(old)
	provider.heads["topic"] = topic
	fixture.nextPass(t)
	with := heads()
	if with[first] != pushed || with[pushed] != pushed || with[topic] != topic {
		t.Fatalf("what came with the two pushes: main %q %q, topic %q", with[first], with[pushed], with[topic])
	}
	if with[old] != "" || len(with) != 6 {
		t.Fatalf("the old history after the pushes: %v", with)
	}
	// A branch that is made of a commit the server holds brings nothing, and
	// a later push on it brings only what is new.
	provider.heads["release"] = pushed
	fixture.nextPass(t)
	later := provider.commit(pushed)
	provider.heads["release"] = later
	fixture.nextPass(t)
	with = heads()
	if with[later] != later || with[pushed] != pushed || with[first] != pushed || len(with) != 7 {
		t.Fatalf("after a branch of a held commit and a push on it: %v", with)
	}
	// What the reader is told reaches the page the Sessions tab reads.
	page, err := fixture.service.ReadRemoteGit(anchoredActor("viewer"), fixture.repo, domain.RemoteCommitCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]string{}
	for _, commit := range page.Commits {
		read[commit.SHA] = commit.Head
	}
	if !reflect.DeepEqual(read, with) {
		t.Fatalf("the page says %v, the store %v", read, with)
	}
}
