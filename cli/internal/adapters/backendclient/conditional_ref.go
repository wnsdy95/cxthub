package backendclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

var _ outbound.ConditionalRefWriter = (*BackendClient)(nil)

func (c *BackendClient) AppendRefConditional(ctx context.Context, repoID string, ref domain.Ref, expectedTarget domain.ContentHash, expectedRevision domain.RepositoryRevision) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	if ref.RepoID != "" && ref.RepoID != repoID {
		return domain.ErrHashMismatch
	}
	if ref.Kind != domain.RefBranch || ref.BranchID == "" {
		return fmt.Errorf("%w: conditional append requires an identified branch", domain.ErrSyncConflict)
	}
	if err := domain.ValidateContentHash(expectedTarget); err != nil {
		return err
	}
	body := struct {
		BranchID         string             `json:"branch_id"`
		Target           domain.ContentHash `json:"target"`
		ExpectedTarget   domain.ContentHash `json:"expected_target"`
		ExpectedRevision struct {
			Graph    uint64 `json:"graph,string"`
			Evidence uint64 `json:"evidence,string"`
			Pending  uint64 `json:"pending,string"`
		} `json:"expected_revision"`
	}{BranchID: ref.BranchID, Target: ref.Target, ExpectedTarget: expectedTarget}
	body.ExpectedRevision.Graph = expectedRevision.Graph
	body.ExpectedRevision.Evidence = expectedRevision.Evidence
	body.ExpectedRevision.Pending = expectedRevision.Pending
	path := c.reposPath(repoID) + "/conditional-appends/v1/" + escapePathName(ref.Name)
	return c.do(ctx, http.MethodPost, path, body, nil)
}
