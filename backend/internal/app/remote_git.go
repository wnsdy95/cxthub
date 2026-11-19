package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	// remoteGitPassBudget is the number of provider requests one pass makes.
	remoteGitPassBudget = 40
	// remoteGitLease is how long a pass holds its claim. A pass ends well
	// inside it: remoteGitPassTimeout bounds the reading, and what is left
	// is for storing how it ended.
	remoteGitLease       = 2 * time.Minute
	remoteGitPassTimeout = 90 * time.Second
	remoteGitTick        = 30 * time.Second
	// remoteGitOpenTimeout bounds applying the publication rule after a pass,
	// which reads every session report of the repository.
	remoteGitOpenTimeout = 60 * time.Second
)

func (s *Service) remoteGitStore() (outbound.RemoteGitStore, error) {
	store, ok := s.meta.(outbound.RemoteGitStore)
	if !ok {
		return nil, fmt.Errorf("%w: remote Git storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// ReadRemoteGit returns what the server read from the repository's Git
// hosting provider: a page of the commit list, and with the page from the top
// the default branch, the branches and the pull requests.
//
// What was read from an origin the repository no longer has is not shown: the
// repository then reads as one that was never read.
func (s *Service) ReadRemoteGit(ctx context.Context, repo domain.ContentHash, before domain.RemoteCommitCursor, limit int) (domain.RemoteGitPage, error) {
	if err := validateHashes(repo); err != nil {
		return domain.RemoteGitPage{}, err
	}
	if limit < 1 || limit > domain.RemoteGitPageLimit {
		return domain.RemoteGitPage{}, fmt.Errorf("%w: a page holds 1 to %d commits", domain.ErrValidation, domain.RemoteGitPageLimit)
	}
	return repositoryRead(ctx, s, func(bound context.Context) (domain.RemoteGitPage, error) {
		page := domain.RemoteGitPage{Branches: []domain.RemoteBranch{}, Pulls: []domain.RemotePull{}, Commits: []domain.RemoteCommit{}}
		store, err := s.remoteGitStore()
		if err != nil {
			return domain.RemoteGitPage{}, err
		}
		current, err := s.meta.GetRepo(bound, repo)
		if err != nil {
			return domain.RemoteGitPage{}, err
		}
		state, err := store.GetRemoteGitState(bound, repo)
		if errors.Is(err, domain.ErrNotFound) || (err == nil && state.GitOrigin != current.GitRemoteURL) {
			return page, nil
		}
		if err != nil {
			return domain.RemoteGitPage{}, err
		}
		if before.IsZero() {
			page.DefaultBranch, page.SyncedAt = state.DefaultBranch, state.SyncedAt
			if page.Branches, err = store.ListRemoteBranches(bound, repo); err != nil {
				return domain.RemoteGitPage{}, err
			}
			if page.Pulls, err = store.ListRemotePulls(bound, repo); err != nil {
				return domain.RemoteGitPage{}, err
			}
		}
		// One more than the page shows whether another page follows.
		commits, err := store.ListRemoteCommits(bound, repo, before, limit+1)
		if err != nil {
			return domain.RemoteGitPage{}, err
		}
		if len(commits) > limit {
			commits = commits[:limit]
			next := domain.RemoteCommitCursorAt(commits[limit-1])
			page.Next = &next
		}
		page.Commits = commits
		return page, nil
	})
}

// RequestRemoteGitRefresh asks for the provider to be read again soon. It
// reads nothing itself: the worker that reads the provider does. The request
// does not move the repository revision.
func (s *Service) RequestRemoteGitRefresh(ctx context.Context, repo domain.ContentHash) error {
	if err := validateHashes(repo); err != nil {
		return err
	}
	ctx = context.WithValue(auditOperation(ctx, "remote_git.refresh_requested"), revisionScopeKey{}, "none")
	return repositoryWriteError(ctx, s, repo, func(bound context.Context) error {
		store, err := s.remoteGitStore()
		if err != nil {
			return err
		}
		current, err := s.meta.GetRepo(bound, repo)
		if err != nil {
			return err
		}
		if current.GitRemoteURL == "" {
			return fmt.Errorf("%w: the repository has no Git origin to read", domain.ErrConflict)
		}
		return store.RequestRemoteGitSync(bound, repo, time.Now().UTC())
	})
}

var _ inbound.RemoteGit = (*Service)(nil)

// requestRemoteGitSync asks for a fresh read of the provider inside the
// transaction that stored a push observation, so that what was pushed is read
// soon. A store without the cache asks for nothing.
func requestRemoteGitSync(tx context.Context, meta any, repo domain.ContentHash, at time.Time) error {
	store, ok := meta.(outbound.RemoteGitStore)
	if !ok {
		return nil
	}
	return store.RequestRemoteGitSync(tx, repo, at)
}

// RemoteGitProvider is what a pass reads from the hosting provider.
type RemoteGitProvider interface {
	outbound.RemoteGitReader
	ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error)
}

type RemoteGitSyncConfig struct {
	Metadata     outbound.MetadataStore
	Blobs        outbound.BlobStore
	Repositories outbound.RepositoryStore
	Reader       RemoteGitProvider
}

// RemoteGitSync reads repositories' Git hosting providers into the server's
// own store. It owns no context ref and never moves the repository revision.
type RemoteGitSync struct {
	work   *repositoryWork
	store  outbound.RemoteGitStore
	reader RemoteGitProvider
	// budget is the number of provider requests one pass makes.
	budget int
	now    func() time.Time
}

func NewRemoteGitSync(config RemoteGitSyncConfig) (*RemoteGitSync, error) {
	store, _ := config.Metadata.(outbound.RemoteGitStore)
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(store) || !rootAdapterPresent(config.Reader) ||
		config.Blobs != nil && !rootAdapterPresent(config.Blobs) || config.Repositories != nil && !rootAdapterPresent(config.Repositories) {
		return nil, domain.ErrValidation
	}
	work := newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))
	return &RemoteGitSync{work: work, store: store, reader: config.Reader, budget: remoteGitPassBudget, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (worker *RemoteGitSync) workerContext(ctx context.Context) context.Context {
	return (gitChangeScope{work: worker.work}).WorkerContext(inbound.WithSystemActor(ctx))
}

// write makes one change of the cache in a repository transaction that does
// not move the repository revision. It refuses the change when the
// repository's origin is no longer the one the pass reads.
func (worker *RemoteGitSync) write(ctx context.Context, repo domain.ContentHash, origin string, change func(context.Context) error) error {
	return repositoryWriteErrorWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), worker.work, repo, func(tx context.Context) error {
		current, err := worker.work.meta.GetRepo(tx, repo)
		if err != nil {
			return err
		}
		if current.GitRemoteURL != origin {
			return domain.ErrConflict
		}
		return change(tx)
	})
}

// SyncRemoteGit runs one pass over the repository's origin when one is due.
// It returns ErrNotFound when no pass is due, another worker holds the claim,
// or the repository has no origin.
//
// A pass that fails keeps what it stored and is tried again later; what it
// read is never taken back, because every write of a pass can be repeated.
func (worker *RemoteGitSync) SyncRemoteGit(ctx context.Context, repo domain.ContentHash) error {
	ctx = worker.workerContext(ctx)
	current, err := (gitChangeScope{work: worker.work}).ReadRepository(ctx, repo)
	if err != nil {
		return err
	}
	if current.GitRemoteURL == "" {
		return domain.ErrNotFound
	}
	pass := &remoteGitPass{sync: worker, repo: repo, origin: current.GitRemoteURL, budget: worker.budget}
	if err := worker.write(ctx, repo, pass.origin, func(tx context.Context) error {
		claimed, err := worker.store.ClaimRemoteGitSync(tx, repo, pass.origin, worker.now(), remoteGitLease)
		pass.state = claimed
		return err
	}); err != nil {
		return err
	}
	work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, repo), remoteGitPassTimeout)
	failure := pass.run(work)
	cancel()
	if failure != nil {
		// The class comes from a fixed vocabulary: provider errors can carry URLs.
		log.Printf("Remote Git read %s deferred: class=%s", repo, gitObservationFailureClass(failure))
	}
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	if err := worker.finish(finish, pass, failure != nil); err != nil || failure != nil {
		return errors.Join(failure, err)
	}
	if err := worker.openSessions(ctx, repo); err != nil {
		log.Printf("Remote Git read %s: opening sessions deferred: class=%s", repo, gitObservationFailureClass(err))
	}
	return nil
}

// finish stores how the pass ended and releases its claim. A pass that no
// longer holds the claim stores nothing: its lease ended and another pass
// started, or the cache was started over. A read asked for while the pass ran
// is carried over from what the store holds.
func (worker *RemoteGitSync) finish(ctx context.Context, pass *remoteGitPass, failed bool) error {
	return repositoryWriteErrorWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), worker.work, pass.repo, func(tx context.Context) error {
		stored, err := worker.store.GetRemoteGitState(tx, pass.repo)
		if err != nil {
			return err
		}
		if !stored.SamePass(pass.state) {
			return fmt.Errorf("%w: the pass no longer holds its claim", domain.ErrConflict)
		}
		next := pass.state
		next.RequestedAt = stored.RequestedAt
		return worker.store.FinishRemoteGitSync(tx, next.Ended(*pass.state.AttemptedAt, failed, pass.cut))
	})
}

// openSessions applies the publication rule after a pass that finished: what
// was read may open parts of sessions to the team, a commit that is now on
// the remote or a branch that was pushed. It has its own transaction and
// time, so that a repository with many sessions does not hold the pass's
// state; a failure is tried again after the next pass, since the rule only
// adds what the facts support.
func (worker *RemoteGitSync) openSessions(ctx context.Context, repo domain.ContentHash) error {
	work, done := context.WithTimeout(context.WithoutCancel(ctx), remoteGitOpenTimeout)
	defer done()
	return repositoryWriteErrorWithWork(context.WithValue(work, revisionScopeKey{}, "none"), worker.work, repo, func(tx context.Context) error {
		return openAnchoredSessions(tx, worker.work.meta, repo, nil, worker.now())
	})
}

// remoteGitPass is one read of one repository's provider.
type remoteGitPass struct {
	sync   *RemoteGitSync
	repo   domain.ContentHash
	origin string
	state  domain.RemoteGitState
	// budget is the number of provider requests the pass may still make.
	budget int
	// cut: the budget ran out with work left.
	cut bool
}

// spend takes one provider request from the budget. When none is left there
// is work the pass cannot do, and the pass is cut.
func (pass *remoteGitPass) spend() bool {
	if pass.budget == 0 {
		pass.cut = true
		return false
	}
	pass.budget--
	return true
}

func (pass *remoteGitPass) run(ctx context.Context) error {
	if !pass.spend() {
		return nil
	}
	branch, err := pass.sync.reader.ReadRemoteDefaultBranch(ctx, pass.origin)
	if err != nil {
		return err
	}
	pass.state.DefaultBranch = branch
	heads, err := pass.branches(ctx)
	if err != nil {
		return err
	}
	missing, err := pass.sync.store.MissingRemoteCommits(ctx, pass.repo, heads)
	if err != nil {
		return err
	}
	for _, head := range missing {
		if err := pass.walk(ctx, head); err != nil {
			return err
		}
	}
	if err := pass.pulls(ctx); err != nil {
		return err
	}
	return pass.frontier(ctx)
}

// branches reads every page of branch heads and, only when all pages were
// read, makes them the stored list. It returns the heads to walk from: the
// default branch first, then the others by name. A list that was not read to
// its end is not stored and gives no heads.
func (pass *remoteGitPass) branches(ctx context.Context) ([]string, error) {
	// The provider's pages are not one snapshot: a name can show on two
	// pages when the list moves between the reads. The later read wins.
	listed := map[string]string{}
	for page := 1; ; page++ {
		if !pass.spend() {
			return nil, nil
		}
		heads, more, err := pass.sync.reader.ListGitHeads(ctx, pass.origin, page)
		if err != nil {
			return nil, err
		}
		for _, head := range heads {
			listed[strings.TrimPrefix(head.Ref, "refs/heads/")] = head.Commit
		}
		if !more {
			break
		}
	}
	branches := make([]domain.RemoteBranch, 0, len(listed))
	for name, sha := range listed {
		branches = append(branches, domain.RemoteBranch{Name: name, SHA: sha})
	}
	sort.Slice(branches, func(left, right int) bool {
		if first, second := branches[left].Name == pass.state.DefaultBranch, branches[right].Name == pass.state.DefaultBranch; first != second {
			return first
		}
		return branches[left].Name < branches[right].Name
	})
	// A branch whose newest commit is another one than at the read before
	// was pushed in between. The first read finds every branch new and says
	// nothing about when any of them was pushed.
	previous, err := pass.sync.store.ListRemoteBranches(ctx, pass.repo)
	if err != nil {
		return nil, err
	}
	var pushes []domain.RemoteBranchPush
	if len(previous) > 0 || pass.state.SyncedAt != nil {
		pushes = domain.RemoteBranchPushes(previous, branches, pass.sync.now())
	}
	if err := pass.sync.write(ctx, pass.repo, pass.origin, func(tx context.Context) error {
		if log, ok := pass.sync.store.(outbound.RemotePushStore); ok {
			if err := log.RecordRemotePushes(tx, pass.repo, pushes); err != nil {
				return err
			}
		}
		return pass.sync.store.ReplaceRemoteBranches(tx, pass.repo, branches)
	}); err != nil {
		return nil, err
	}
	heads := make([]string, 0, len(branches))
	for _, branch := range branches {
		heads = append(heads, branch.SHA)
	}
	return heads, nil
}

// errRemoteGitUnreadable: the provider does not list the commit a walk
// started from.
var errRemoteGitUnreadable = errors.New("the provider does not list the commit")

// walk reads the commits that lead to sha, a page at a time, and stores each
// page. It stops when the provider has no more, when every commit of a page
// was stored before the page was, or when the budget is gone.
func (pass *remoteGitPass) walk(ctx context.Context, sha string) error {
	for page := 1; ; page++ {
		if !pass.spend() {
			return nil
		}
		commits, more, err := pass.sync.reader.ListRemoteCommits(ctx, pass.origin, sha, page)
		var status outbound.GitProviderStatusError
		if page == 1 && errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusConflict || status.Status == http.StatusUnprocessableEntity) {
			return fmt.Errorf("%w: %w", errRemoteGitUnreadable, err)
		}
		if err != nil {
			return err
		}
		added, err := pass.put(ctx, commits)
		if err != nil {
			return err
		}
		if added == 0 || !more {
			return nil
		}
	}
}

func (pass *remoteGitPass) put(ctx context.Context, commits []domain.RemoteCommit) (int, error) {
	added := 0
	if len(commits) == 0 {
		return 0, nil
	}
	err := pass.sync.write(ctx, pass.repo, pass.origin, func(tx context.Context) error {
		stored, err := pass.sync.store.PutRemoteCommits(tx, pass.repo, commits)
		added = stored
		return err
	})
	return added, err
}

// pulls reads pull requests, the most recently updated first, and stores
// each one that is not stored as the provider now shows it, with its commits.
//
// The first full read goes through every page and remembers the page it is
// on, so that a pass that runs out of budget continues there. After that a
// pass stops at the first page on which nothing changed.
//
// A later pass that runs out of budget among changed pull requests would
// leave the rest unread for good: the pages before them no longer show a
// change, so the next pass would stop there. Such a pass goes back to reading
// every page from the one it was on.
func (pass *remoteGitPass) pulls(ctx context.Context) error {
	if pass.budget == 0 {
		pass.cut = true
		return nil
	}
	stored, err := pass.sync.store.ListRemotePulls(ctx, pass.repo)
	if err != nil {
		return err
	}
	known := make(map[int]domain.RemotePull, len(stored))
	for _, pull := range stored {
		known[pull.Number] = pull
	}
	page := 1
	if !pass.state.PullsComplete && pass.state.PullPage > 1 {
		page = pass.state.PullPage
	}
	// resume makes the next passes read every page from this one on.
	resume := func() {
		if pass.state.PullsComplete {
			pass.state.PullsComplete, pass.state.PullPage = false, page
		}
	}
	for {
		if !pass.spend() {
			// Only a page reached because the one before it changed is known
			// to be worth reading.
			if page > 1 {
				resume()
			}
			return nil
		}
		listed, more, err := pass.sync.reader.ListRemotePulls(ctx, pass.origin, page)
		if err != nil {
			return err
		}
		changed := false
		for _, pull := range listed {
			if had, ok := known[pull.Number]; ok && had.UpdatedAt.Equal(pull.UpdatedAt) && had.HeadSHA == pull.HeadSHA {
				continue
			}
			changed = true
			if read, err := pass.pull(ctx, pull); err != nil || !read {
				if err == nil {
					resume()
				}
				return err
			}
			// The list can move while it is read and show a pull request
			// again on the next page.
			known[pull.Number] = pull
		}
		switch {
		case !more && !pass.state.PullsComplete:
			// A page that is not full is the last one: the page after it
			// would be empty.
			pass.state.PullsComplete, pass.state.PullPage = true, 0
			return nil
		case !more, pass.state.PullsComplete && !changed:
			return nil
		}
		page++
		if !pass.state.PullsComplete {
			pass.state.PullPage = page
		}
	}
}

// pull reads the commit list of a pull request, stores those commits and then
// the pull request. It reports false when the budget ran out first; nothing
// of the pull request is stored then.
func (pass *remoteGitPass) pull(ctx context.Context, pull domain.RemotePull) (bool, error) {
	commits := []domain.RemoteCommit{}
	for page := 1; len(commits) < domain.RemotePullCommitLimit; page++ {
		if !pass.spend() {
			return false, nil
		}
		listed, more, err := pass.sync.reader.ListRemotePullCommits(ctx, pass.origin, pull.Number, page)
		if err != nil {
			return false, err
		}
		commits = append(commits, listed...)
		if !more {
			break
		}
	}
	if len(commits) >= domain.RemotePullCommitLimit {
		commits, pull.Truncated = commits[:domain.RemotePullCommitLimit], true
	}
	pull.Commits = make([]string, 0, len(commits))
	for _, commit := range commits {
		pull.Commits = append(pull.Commits, commit.SHA)
	}
	return true, pass.sync.write(ctx, pass.repo, pass.origin, func(tx context.Context) error {
		if _, err := pass.sync.store.PutRemoteCommits(tx, pass.repo, commits); err != nil {
			return err
		}
		return pass.sync.store.PutRemotePull(tx, pass.repo, pull)
	})
}

// frontier spends what is left of the budget on the parents that stored
// commits name and that are not stored. A parent the provider does not list
// is remembered as unreadable, so that it is not asked for on every pass.
func (pass *remoteGitPass) frontier(ctx context.Context) error {
	// The store skips the commits the stored state lists. Those refused in
	// this pass are not in the stored state yet.
	skip, refused := map[string]struct{}{}, 0
	left := func(limit int) ([]string, error) {
		parents, err := pass.sync.store.RemoteCommitFrontier(ctx, pass.repo, limit+refused)
		if err != nil {
			return nil, err
		}
		readable := parents[:0]
		for _, sha := range parents {
			if _, skipped := skip[sha]; !skipped {
				readable = append(readable, sha)
			}
		}
		return readable, nil
	}
	for pass.budget > 0 {
		parents, err := left(pass.budget)
		if err != nil || len(parents) == 0 {
			return err
		}
		for _, sha := range parents {
			if pass.budget == 0 {
				break
			}
			err := pass.walk(ctx, sha)
			if errors.Is(err, errRemoteGitUnreadable) {
				pass.state = pass.state.WithUnreadable(sha)
				skip[sha] = struct{}{}
				refused++
				continue
			}
			if err != nil {
				return err
			}
		}
	}
	if pass.cut {
		return nil
	}
	// The budget is gone. Whether a parent is still to be read decides when
	// the next pass runs.
	parents, err := left(1)
	pass.cut = len(parents) > 0
	return err
}

// due reports whether a pass over the repository may be due, from what can be
// read without taking anything: the claim decides.
func (worker *RemoteGitSync) due(ctx context.Context, repo domain.Repo) bool {
	state, err := worker.store.GetRemoteGitState(ctx, repo.ID)
	if err != nil {
		return true
	}
	return state.GitOrigin != repo.GitRemoteURL || state.Due(worker.now(), time.Time{})
}

// Process runs a pass over every repository that has an origin and whose
// pass is due. A repository that fails does not stop the others.
func (worker *RemoteGitSync) Process(ctx context.Context) error {
	scoped := worker.workerContext(ctx)
	repos, err := worker.work.meta.ListRepos(scoped, "default")
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if repo.GitRemoteURL == "" || !worker.due(scoped, repo) {
			continue
		}
		// A pass that failed was logged with its class and has its retry time.
		_ = worker.SyncRemoteGit(ctx, repo.ID)
	}
	return nil
}

// Run reads the providers of the repositories whose pass is due until ctx
// ends.
func (worker *RemoteGitSync) Run(ctx context.Context) {
	ticker := time.NewTicker(remoteGitTick)
	defer ticker.Stop()
	for {
		if err := worker.Process(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Remote Git worker: class=%s", gitObservationFailureClass(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
