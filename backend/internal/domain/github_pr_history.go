package domain

import "time"

type GitHubHeadRewrite struct {
	ID        string
	Before    string
	After     string
	CreatedAt time.Time
}

type GitHubPRHistoryPage struct {
	RepositoryID          int64
	HeadRepositoryID      int64
	PullRequestID         string
	PullRequestDatabaseID int64
	Number                int
	BaseBranch            string
	HeadBranch            string
	HeadSHA               string
	MergeSHA              string
	Merged                bool
	MergedAt              time.Time
	Rewrites              []GitHubHeadRewrite
	EndCursor             string
	HasNextPage           bool
}

func (p GitHubPRHistoryPage) Validate() error {
	if p.RepositoryID <= 0 || p.HeadRepositoryID <= 0 || p.PullRequestDatabaseID <= 0 || p.Number <= 0 || p.PullRequestID == "" || len(p.PullRequestID) > 256 || ValidateBranchName(p.BaseBranch) != nil || ValidateBranchName(p.HeadBranch) != nil || ValidateGitOID(p.HeadSHA) != nil || p.Rewrites == nil || len(p.Rewrites) > 100 || len(p.EndCursor) > 4096 || (p.HasNextPage && p.EndCursor == "") {
		return ErrIntegrity
	}
	if p.Merged {
		if ValidateGitOID(p.MergeSHA) != nil || len(p.MergeSHA) != len(p.HeadSHA) || p.MergedAt.IsZero() {
			return ErrIntegrity
		}
	} else if p.MergeSHA != "" || !p.MergedAt.IsZero() {
		return ErrIntegrity
	}
	seen := make(map[string]bool, len(p.Rewrites))
	var previous time.Time
	for _, rewrite := range p.Rewrites {
		if rewrite.ID == "" || len(rewrite.ID) > 256 || seen[rewrite.ID] || rewrite.CreatedAt.IsZero() || rewrite.CreatedAt.Before(previous) {
			return ErrIntegrity
		}
		if !(p.Merged && rewrite.CreatedAt.After(p.MergedAt)) && (ValidateGitOID(rewrite.Before) != nil || ValidateGitOID(rewrite.After) != nil || len(rewrite.Before) != len(p.HeadSHA) || len(rewrite.After) != len(p.HeadSHA) || rewrite.Before == rewrite.After) {
			return ErrIntegrity
		}
		seen[rewrite.ID] = true
		previous = rewrite.CreatedAt
	}
	return nil
}
