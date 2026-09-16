package backendclient

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"net/http"
)

func (c *BackendClient) PushHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.reposPath(e.RepoID)+"/history", e, nil)
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

// Repository reads cloud identity/configuration without consulting local state.
func (c *BackendClient) Repository(ctx context.Context, repoID string) (domain.Repo, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return domain.Repo{}, err
	}
	var repo domain.Repo
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
