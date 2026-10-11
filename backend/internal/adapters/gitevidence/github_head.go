package gitevidence

import (
	"context"
	"net/url"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHeadReader = (*GitHub)(nil)

func (provider *GitHub) ReadGitHead(ctx context.Context, origin, branch string) (outbound.GitHead, error) {
	if err := domain.ValidateBranchName(branch); err != nil {
		return outbound.GitHead{}, err
	}
	repository, err := repositoryPath(origin)
	if err != nil {
		return outbound.GitHead{}, err
	}
	var result struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := provider.get(ctx, "/repos/"+repository+"/git/ref/heads/"+url.PathEscape(branch), &result); err != nil {
		return outbound.GitHead{}, err
	}
	if result.Ref != "refs/heads/"+branch || result.Object.Type != "commit" || domain.ValidateGitOID(result.Object.SHA) != nil {
		return outbound.GitHead{}, domain.ErrIntegrity
	}
	return outbound.GitHead{Ref: result.Ref, Commit: result.Object.SHA}, nil
}
