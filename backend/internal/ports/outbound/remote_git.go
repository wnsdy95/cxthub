package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RemoteGitReader reads commits and pull requests from the repository's Git
// hosting provider. The origin comes from trusted repository metadata, never
// from a caller. Every list is read one page at a time; the second result
// says that the provider has another page.
//
// A commit the provider does not have is reported as a GitProviderStatusError
// with the provider's status, so that a caller can tell it from a failure.
type RemoteGitReader interface {
	// ReadRemoteDefaultBranch returns the name of the default branch.
	ReadRemoteDefaultBranch(ctx context.Context, origin string) (string, error)
	// ListRemoteCommits lists the commits reachable from the commit from,
	// newest first.
	ListRemoteCommits(ctx context.Context, origin, from string, page int) ([]domain.RemoteCommit, bool, error)
	// ListRemotePulls lists pull requests in every state, the most recently
	// updated first, without their commits.
	ListRemotePulls(ctx context.Context, origin string, page int) ([]domain.RemotePull, bool, error)
	// ListRemotePullCommits lists the commits of a pull request, oldest first.
	ListRemotePullCommits(ctx context.Context, origin string, number, page int) ([]domain.RemoteCommit, bool, error)
}

// RemoteGitStore keeps what the server read from a repository's Git hosting
// provider, and the state of that reading. All of it belongs to one origin.
type RemoteGitStore interface {
	// GetRemoteGitState returns the reading state, or ErrNotFound when the
	// repository was never read.
	GetRemoteGitState(ctx context.Context, repo domain.ContentHash) (domain.RemoteGitState, error)
	// ClaimRemoteGitSync gives one worker the next pass over origin for the
	// length of lease, and returns the state the pass starts with. It creates
	// the state when there is none. When what is stored was read from another
	// origin, it removes everything stored for the repository and starts
	// over. It returns ErrNotFound when no pass is due or another worker
	// holds the claim.
	ClaimRemoteGitSync(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.RemoteGitState, error)
	// FinishRemoteGitSync stores the state and releases the claim.
	FinishRemoteGitSync(ctx context.Context, state domain.RemoteGitState) error
	// ListenRemoteGitRequests begins to listen: from when it returns, asked
	// is called with the repository of every request for a read that is
	// committed, whichever process stored it, until stop is called. stop
	// returns when nothing is told any more. asked must not wait. A request
	// stored while nothing listened, or while the store had lost what it
	// listens with, is not told later: its repository is due, and the look
	// for due repositories finds it.
	ListenRemoteGitRequests(asked func(domain.ContentHash)) (stop func(), err error)
	// RequestRemoteGitSync records that a fresh read was asked for at now. It
	// does nothing when the repository was never read.
	RequestRemoteGitSync(ctx context.Context, repo domain.ContentHash, now time.Time) error

	// PutRemoteCommits stores the commits that are not stored yet and returns
	// how many those were. A stored commit is never changed.
	PutRemoteCommits(ctx context.Context, repo domain.ContentHash, commits []domain.RemoteCommit) (added int, err error)
	// MissingRemoteCommits returns the commit numbers among shas that are not
	// stored, in the order given.
	MissingRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]string, error)
	// GetRemoteCommits returns the stored commits among shas, in the order
	// given.
	GetRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]domain.RemoteCommit, error)
	// ListRemoteCommits lists commits by commit time, newest first, then by
	// commit number, largest first, starting after before. The cursor of a
	// page is its last commit.
	ListRemoteCommits(ctx context.Context, repo domain.ContentHash, before domain.RemoteCommitCursor, limit int) ([]domain.RemoteCommit, error)
	// SearchRemoteCommits lists the stored commits whose subject contains
	// query, compared without regard to letter case, or whose commit number
	// begins with it, by commit time, newest first. An empty query finds
	// nothing.
	SearchRemoteCommits(ctx context.Context, repo domain.ContentHash, query string, limit int) ([]domain.RemoteCommit, error)
	// RemoteCommitFrontier returns parents that stored commits name and that
	// are not stored, without the commits the state lists as unreadable, in
	// the order of their numbers.
	RemoteCommitFrontier(ctx context.Context, repo domain.ContentHash, limit int) ([]string, error)
	// FindRemoteCommitCopies returns the stored commits that have one of the
	// keys, in the order of their numbers.
	FindRemoteCommitCopies(ctx context.Context, repo domain.ContentHash, keys []domain.RemoteCommitKey) ([]domain.RemoteCommit, error)

	// ReplaceRemoteBranches makes branches the whole list of branches.
	ReplaceRemoteBranches(ctx context.Context, repo domain.ContentHash, branches []domain.RemoteBranch) error
	// ListRemoteBranches lists the branches by name.
	ListRemoteBranches(ctx context.Context, repo domain.ContentHash) ([]domain.RemoteBranch, error)

	// PutRemotePull stores a pull request, replacing the one with the same
	// number together with its commit list.
	PutRemotePull(ctx context.Context, repo domain.ContentHash, pull domain.RemotePull) error
	// ListRemotePulls lists the pull requests with their commit lists, the
	// highest number first.
	ListRemotePulls(ctx context.Context, repo domain.ContentHash) ([]domain.RemotePull, error)
}

// RemotePushStore keeps when the server saw each branch of the remote pushed.
// It belongs to the same origin as the rest of what was read from the
// provider and is removed with it.
type RemotePushStore interface {
	// RecordRemotePushes adds pushes: one for each time a branch was seen
	// pushed. The same branch at the same time again adds nothing.
	RecordRemotePushes(ctx context.Context, repo domain.ContentHash, pushes []domain.RemoteBranchPush) error
	// ListRemotePushes lists the pushes of the named branches, oldest first.
	ListRemotePushes(ctx context.Context, repo domain.ContentHash, branches []string) ([]domain.RemoteBranchPush, error)
}
