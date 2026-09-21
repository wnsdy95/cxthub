package gitevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitSyncReader = (*GitHub)(nil)

func (g *GitHub) ReadAuditCommit(ctx context.Context, origin, sha string) (string, error) {
	repo, err := repositoryPath(origin)
	if err != nil {
		return "", err
	}
	obj, err := g.commit(ctx, repo, sha)
	return obj.SHA, err
}
func (g *GitHub) ReadAuditPR(ctx context.Context, origin string, number int) (domain.PullRequestMerge, bool, error) {
	var result domain.PullRequestMerge
	repo, err := repositoryPath(origin)
	if err != nil {
		return result, false, err
	}
	if number < 1 {
		return result, false, domain.ErrValidation
	}
	var p struct {
		Number   int    `json:"number"`
		Merged   *bool  `json:"merged"`
		MergeSHA string `json:"merge_commit_sha"`
		Base     struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err = g.get(ctx, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), &p); err != nil {
		return result, false, err
	}
	if p.Number != number || p.Merged == nil || !strings.EqualFold(p.Base.Repo.FullName, repo) {
		return result, false, fmt.Errorf("%w: invalid GitHub PR identity", domain.ErrIntegrity)
	}
	result = domain.PullRequestMerge{Number: p.Number, BaseBranch: p.Base.Ref, HeadBranch: p.Head.Ref, HeadSHA: p.Head.SHA, MergeSHA: p.MergeSHA}
	if *p.Merged {
		if err = validateAuditPR(result); err != nil {
			return result, false, domain.ErrIntegrity
		}
	}
	return result, *p.Merged, nil
}

func (g *GitHub) ListAuditPRs(ctx context.Context, origin string, page int) (outbound.GitAuditPRPage, error) {
	if page < 1 {
		return outbound.GitAuditPRPage{}, domain.ErrValidation
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return outbound.GitAuditPRPage{}, err
	}
	var rows []struct {
		UpdatedAt string  `json:"updated_at"`
		Number    int     `json:"number"`
		MergedAt  *string `json:"merged_at"`
		MergeSHA  string  `json:"merge_commit_sha"`
		Base      struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err = g.get(ctx, "/repos/"+repo+"/pulls?state=closed&sort=updated&direction=desc&per_page=20&page="+strconv.Itoa(page), &rows); err != nil {
		return outbound.GitAuditPRPage{}, err
	}
	if rows == nil || len(rows) > 20 {
		return outbound.GitAuditPRPage{}, domain.ErrIntegrity
	}
	result := []domain.PullRequestMerge{}
	for _, row := range rows {
		if row.MergedAt == nil {
			continue
		}
		if !strings.EqualFold(row.Base.Repo.FullName, repo) {
			return outbound.GitAuditPRPage{}, domain.ErrIntegrity
		}
		pr := domain.PullRequestMerge{Number: row.Number, BaseBranch: row.Base.Ref, HeadBranch: row.Head.Ref, HeadSHA: row.Head.SHA, MergeSHA: row.MergeSHA}
		if err = validateAuditPR(pr); err != nil {
			return outbound.GitAuditPRPage{}, domain.ErrIntegrity
		}
		result = append(result, pr)
	}
	raw, _ := json.Marshal(rows)
	sum := sha256.Sum256(raw)
	return outbound.GitAuditPRPage{PRs: result, More: len(rows) == 20, Count: len(rows), Anchor: hex.EncodeToString(sum[:])}, nil
}

// Fork PRs can have the same head/base branch name; audit facts are broader
// than the command that promotes an in-repository context branch.
func validateAuditPR(pr domain.PullRequestMerge) error {
	if pr.Number < 1 {
		return domain.ErrIntegrity
	}
	for _, name := range []string{pr.BaseBranch, pr.HeadBranch} {
		if domain.ValidateBranchName(name) != nil {
			return domain.ErrIntegrity
		}
	}
	for _, sha := range []string{pr.HeadSHA, pr.MergeSHA} {
		if domain.ValidateGitOID(sha) != nil {
			return domain.ErrIntegrity
		}
	}
	return nil
}
