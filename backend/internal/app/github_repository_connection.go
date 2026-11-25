package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepositoryConnection says whether the server reads a repository on GitHub
// through an installation of the GitHub App at this moment, and names the
// namespace whose owners decide that. A binding is accepted by the rule the
// reader of GitHub is given its credential by, so what a reader of the
// repository is told is what the server does.
func (worker *GitHubWorker) RepositoryConnection(ctx context.Context, repoID domain.ContentHash) (bool, string, error) {
	repo, err := worker.metadata.GetRepo(ctx, repoID)
	if err != nil {
		return false, "", err
	}
	if repo.RepositoryID == "" {
		return false, "", nil
	}
	local, err := worker.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return false, "", err
	}
	connection, err := worker.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, local.OwnerNamespaceID, nil
	}
	if err != nil {
		return false, "", err
	}
	for _, binding := range connection.Bindings {
		if binding.ContextRepoID != repoID {
			continue
		}
		if _, err = worker.binding(ctx, connection, binding); err == nil {
			return true, local.OwnerNamespaceID, nil
		}
		if !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrConflict) {
			return false, "", err
		}
	}
	return false, local.OwnerNamespaceID, nil
}

// RepositoryConnection answers for the API server, which holds no worker of
// its own.
func (connections *GitHubConnections) RepositoryConnection(ctx context.Context, repoID domain.ContentHash) (bool, string, error) {
	return connections.worker().RepositoryConnection(ctx, repoID)
}
