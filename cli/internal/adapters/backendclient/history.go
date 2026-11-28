package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// Repository reads cloud identity/configuration without consulting local state.
func (c *BackendClient) Repository(ctx context.Context, repoID string) (domain.Repo, error) {
	var repo domain.Repo
	if err := domain.ValidateContentHash(repoID); err != nil {
		return repo, err
	}
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID), nil, &repo); err != nil {
		return repo, err
	}
	if repo.ID != repoID {
		return repo, domain.ErrHashMismatch
	}
	if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
		return repo, err
	}
	return repo, nil
}
