package backendclient

import (
	"context"
	"errors"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ReadGitHubConnection reads, with GET /repos/{repoID}/github-connection,
// whether the server reads the repository on GitHub through the GitHub App.
// A server from before that question has no such route and answers 404,
// which is read as a server that does not say.
func (c *BackendClient) ReadGitHubConnection(ctx context.Context, repoID string) (domain.GitHubRepositoryConnection, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.GitHubRepositoryConnection{}, err
	}
	var connection domain.GitHubRepositoryConnection
	err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/github-connection", nil, &connection, 1<<20)
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() == http.StatusNotFound {
		return domain.GitHubRepositoryConnection{}, nil
	}
	if err != nil {
		return domain.GitHubRepositoryConnection{}, err
	}
	return connection, nil
}
