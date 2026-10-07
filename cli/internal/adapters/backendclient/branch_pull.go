package backendclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// PullSelectedBranchTo is called only after the authorized repository response
// advertises this protocol. Route/object errors never trigger a broad fallback.
func (c *BackendClient) PullSelectedBranchTo(ctx context.Context, repo string, request domain.BranchPullRequest, states map[domain.ContentHash]domain.ContentHash, haves []domain.ContentHash, receiver outbound.PullDocumentReceiver) (domain.BranchPullPlan, []domain.Snapshot, error) {
	var plan domain.BranchPullPlan
	if err := request.Validate(); err != nil {
		return plan, nil, err
	}
	if receiver == nil {
		return plan, nil, fmt.Errorf("pull document receiver is required")
	}
	catalog := func(ctx context.Context, repo string) (domain.Manifest, error) {
		if err := c.do(ctx, http.MethodPost, c.reposPath(repo)+"/pull/branch-plan", request, &plan); err != nil {
			return domain.Manifest{}, err
		}
		if err := domain.ValidateBranchPullPlan(repo, request, plan); err != nil {
			return domain.Manifest{}, err
		}
		return domain.Manifest{RepoID: repo, ContextProtocol: plan.ContextProtocol, Refs: plan.Refs, SnapshotIndex: plan.SnapshotIndex, SnapshotStates: plan.SnapshotStates}, nil
	}
	snaps, _, _, err := c.pullCatalog(ctx, repo, states, haves, receiver, request.Branch, catalog, false)
	if err != nil {
		return domain.BranchPullPlan{}, nil, err
	}
	return plan, snaps, nil
}
