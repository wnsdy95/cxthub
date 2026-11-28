package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepositoryConnection says whether the server reads a repository on GitHub
// through an installation of the GitHub App at this moment. A binding is
// accepted by the rule the reader of GitHub is given its credential by, so
// what a reader of the repository is told is what the server does.
func (worker *GitHubWorker) RepositoryConnection(ctx context.Context, repoID domain.ContentHash) (bool, error) {
	if _, err := worker.metadata.GetRepo(ctx, repoID); err != nil {
		return false, err
	}
	connection, err := worker.store.FindGitHubBinding(ctx, repoID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, binding := range connection.Bindings {
		if binding.ContextRepoID != repoID {
			continue
		}
		if _, err = worker.binding(ctx, connection, binding); err == nil {
			return true, nil
		}
		if !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrConflict) {
			return false, err
		}
	}
	return false, nil
}

// RepositoryConnection answers for the API server, which holds no worker of
// its own.
func (connections *GitHubConnections) RepositoryConnection(ctx context.Context, repoID domain.ContentHash) (bool, error) {
	return connections.worker().RepositoryConnection(ctx, repoID)
}
