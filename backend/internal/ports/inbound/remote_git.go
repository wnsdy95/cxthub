package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RemoteGit reads what the server keeps of a repository's commits, branches
// and pull requests as its Git hosting provider shows them, and asks for a
// fresh read of the provider.
type RemoteGit interface {
	// ReadRemoteGit returns a page of the commit list that starts after
	// before. The page from the top of the list also carries the default
	// branch, the branches and the pull requests.
	ReadRemoteGit(ctx context.Context, repo domain.ContentHash, before domain.RemoteCommitCursor, limit int) (domain.RemoteGitPage, error)
	// RequestRemoteGitRefresh asks for the provider to be read again soon.
	RequestRemoteGitRefresh(ctx context.Context, repo domain.ContentHash) error
}
