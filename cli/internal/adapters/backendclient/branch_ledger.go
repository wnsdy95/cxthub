package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// RegisterBranchIDs reports, with PUT /repos/{repoID}/branch-ids, the IDs
// this CLI keeps for branches Git's records show on origin. The server joins
// each ID to the lifetime the name has, or opens one, and answers which.
func (c *BackendClient) RegisterBranchIDs(ctx context.Context, repoID string, branches []domain.BranchRegistration) ([]domain.BranchRegistrationResult, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	if branches == nil {
		branches = []domain.BranchRegistration{}
	}
	body := struct {
		Branches []domain.BranchRegistration `json:"branches"`
	}{Branches: branches}
	var out struct {
		Branches []domain.BranchRegistrationResult `json:"branches"`
	}
	if err := c.doLimited(ctx, http.MethodPut, c.reposPath(repoID)+"/branch-ids", body, &out, 4<<20); err != nil {
		return nil, err
	}
	return out.Branches, nil
}
