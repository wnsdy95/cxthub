package backendclient

import (
	"context"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"net/http"
	"net/url"
)

// repositoryView includes runtime capabilities without persisting them as
// local repository configuration.
type repositoryView struct {
	domain.Repo
	CatalogMerkleVersion   int  `json:"catalog_merkle_version,omitempty"`
	CatalogVersion         int  `json:"catalog_version,omitempty"`
	BranchPullVersion      int  `json:"branch_pull_version,omitempty"`
	InitialAnchorAvailable bool `json:"initial_anchor_available,omitempty"`
}

func (c *BackendClient) readRepository(ctx context.Context, repoID, initialBranch string) (repositoryView, error) {
	var view repositoryView
	if err := domain.ValidateContentHash(repoID); err != nil {
		return view, err
	}
	path := c.reposPath(repoID)
	if initialBranch != "" {
		if err := domain.ValidateBranchName(initialBranch); err != nil {
			return view, err
		}
		path += "?initial_branch=" + url.QueryEscape(initialBranch)
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &view); err != nil {
		return view, err
	}
	if view.ID != repoID {
		return view, domain.ErrHashMismatch
	}
	if err := domain.ValidateBranchName(view.DefaultBranch); err != nil {
		return view, err
	}
	return view, nil
}

// Repository reads cloud identity/configuration without consulting local state.
func (c *BackendClient) Repository(ctx context.Context, repoID string) (domain.Repo, error) {
	view, err := c.readRepository(ctx, repoID, "")
	return view.Repo, err
}
