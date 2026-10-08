package backendclient

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"net/http"
	"net/url"
)

func (c *BackendClient) PushHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	path := c.reposPath(e.RepoID) + "/history"
	if e.MemorySelectionParent != "" {
		// Fence rolling old nodes that silently discard unknown history fields.
		// An unsupported route is an error; never retry through ordinary history.
		path += "/memory-selection"
	}
	return c.do(ctx, http.MethodPost, path, e, nil)
}
func (c *BackendClient) PullHistoryEvents(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	var events []domain.HistoryEvent
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/history", nil, &events); err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.RepoID != repoID {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (c *BackendClient) PromotePullRequest(ctx context.Context, repoID string, pr domain.PullRequestMerge) (domain.Ref, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.Ref{}, err
	}
	if err := pr.Validate(); err != nil {
		return domain.Ref{}, err
	}
	var out struct {
		Ref domain.Ref `json:"ref"`
	}
	err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/prs/promote", pr, &out)
	return out.Ref, err
}

// repositoryView includes runtime capabilities without persisting them as
// local repository configuration.
type repositoryView struct {
	domain.Repo
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

func (c *BackendClient) PullCapabilities(ctx context.Context, repoID string) (outbound.PullCapabilities, error) {
	view, err := c.readRepository(ctx, repoID, "")
	return outbound.PullCapabilities{ContextProtocol: view.ContextProtocol, BranchPlanVersion: view.BranchPullVersion}, err
}

// ContextProtocol reads small repository metadata, not the full object catalog.
func (c *BackendClient) ContextProtocol(ctx context.Context, repoID string) (int, error) {
	repo, err := c.Repository(ctx, repoID)
	if err != nil {
		return 0, err
	}
	return repo.ContextProtocol, nil
}

func (c *BackendClient) SubmitPRPromotion(ctx context.Context, repo string, pr domain.PullRequestMerge) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return err
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	var accepted struct {
		Repo domain.ContentHash      `json:"repo_id"`
		PR   domain.PullRequestMerge `json:"pr"`
	}
	if err := c.do(ctx, http.MethodPost, c.reposPath(repo)+"/prs/promotions", pr, &accepted); err != nil {
		return err
	}
	if string(accepted.Repo) != repo || accepted.PR != pr {
		return domain.ErrHashMismatch
	}
	return nil
}
