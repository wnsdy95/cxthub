package gitevidence

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RemoteGitReader = (*GitHub)(nil)

const remoteGitPageSize = 100

// malformedRemoteGit names provider data that is not stored as it came.
// Provider text never becomes part of the error.
func malformedRemoteGit(what string) error {
	return fmt.Errorf("%w: malformed provider %s", domain.ErrIntegrity, what)
}

// remoteGitTime reads a provider time, which Git keeps to the second.
func remoteGitTime(raw string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC().Truncate(time.Second), true
}

// ReadRemoteDefaultBranch reads the repository itself. The credential
// resolver admits reads below a repository, so the read is resolved for the
// repository's own scope and the provider is asked for what that scope names.
func (g *GitHub) ReadRemoteDefaultBranch(ctx context.Context, origin string) (string, error) {
	repo, err := repositoryPath(origin)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token, bucket, scope, err := g.authorize(ctx, "/repos/"+repo+"/")
	if err != nil {
		return "", err
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.fetch(ctx, token, bucket, strings.TrimSuffix(scope, "/"), &out); err != nil {
		return "", err
	}
	if !domain.ValidRemoteBranchName(out.DefaultBranch) {
		return "", malformedRemoteGit("default branch")
	}
	return out.DefaultBranch, nil
}

type remoteCommitRow struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
		Committer *struct {
			Date string `json:"date"`
		} `json:"committer"`
		Message string `json:"message"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// remoteCommits turns one page of the provider's commit list into commits. A
// row that is not a whole commit fails the page: nothing of it is stored.
func remoteCommits(rows []remoteCommitRow) ([]domain.RemoteCommit, bool, error) {
	if rows == nil || len(rows) > remoteGitPageSize {
		return nil, false, malformedRemoteGit("commit list")
	}
	commits := make([]domain.RemoteCommit, 0, len(rows))
	for _, row := range rows {
		if row.Commit.Author == nil || row.Commit.Committer == nil || row.Parents == nil {
			return nil, false, malformedRemoteGit("commit")
		}
		authored, authoredOK := remoteGitTime(row.Commit.Author.Date)
		committed, committedOK := remoteGitTime(row.Commit.Committer.Date)
		if !authoredOK || !committedOK {
			return nil, false, malformedRemoteGit("commit")
		}
		commit := domain.RemoteCommit{SHA: row.SHA, Parents: make([]string, 0, len(row.Parents)),
			AuthorName: row.Commit.Author.Name, AuthorEmail: row.Commit.Author.Email,
			AuthoredAt: authored, CommittedAt: committed, Subject: domain.RemoteCommitSubject(row.Commit.Message)}
		for _, parent := range row.Parents {
			commit.Parents = append(commit.Parents, parent.SHA)
		}
		if commit.Validate() != nil {
			return nil, false, malformedRemoteGit("commit")
		}
		commits = append(commits, commit)
	}
	return commits, len(rows) == remoteGitPageSize, nil
}

func (g *GitHub) ListRemoteCommits(ctx context.Context, origin, from string, page int) ([]domain.RemoteCommit, bool, error) {
	if page < 1 || !domain.ValidRemoteCommitSHA(from) {
		return nil, false, domain.ErrValidation
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return nil, false, err
	}
	var rows []remoteCommitRow
	if err := g.get(ctx, "/repos/"+repo+"/commits?sha="+from+"&per_page=100&page="+strconv.Itoa(page), &rows); err != nil {
		return nil, false, err
	}
	return remoteCommits(rows)
}

func (g *GitHub) ListRemotePullCommits(ctx context.Context, origin string, number, page int) ([]domain.RemoteCommit, bool, error) {
	if page < 1 || number < 1 {
		return nil, false, domain.ErrValidation
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return nil, false, err
	}
	var rows []remoteCommitRow
	if err := g.get(ctx, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/commits?per_page=100&page="+strconv.Itoa(page), &rows); err != nil {
		return nil, false, err
	}
	return remoteCommits(rows)
}

// ListRemotePulls lists pull requests without their commits. A pull request
// is merged when the provider gives it a merge time; only then does it name
// the commit it became.
func (g *GitHub) ListRemotePulls(ctx context.Context, origin string, page int) ([]domain.RemotePull, bool, error) {
	if page < 1 {
		return nil, false, domain.ErrValidation
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return nil, false, err
	}
	type side struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	}
	var rows []struct {
		Number    int     `json:"number"`
		Title     string  `json:"title"`
		State     string  `json:"state"`
		MergedAt  *string `json:"merged_at"`
		MergeSHA  string  `json:"merge_commit_sha"`
		UpdatedAt string  `json:"updated_at"`
		Head      side    `json:"head"`
		Base      side    `json:"base"`
	}
	if err := g.get(ctx, "/repos/"+repo+"/pulls?state=all&sort=updated&direction=desc&per_page=100&page="+strconv.Itoa(page), &rows); err != nil {
		return nil, false, err
	}
	if rows == nil || len(rows) > remoteGitPageSize {
		return nil, false, malformedRemoteGit("pull request list")
	}
	pulls := make([]domain.RemotePull, 0, len(rows))
	for _, row := range rows {
		updated, ok := remoteGitTime(row.UpdatedAt)
		if !ok || row.Base.Repo == nil {
			return nil, false, malformedRemoteGit("pull request")
		}
		pull := domain.RemotePull{Number: row.Number, Title: row.Title, State: row.State, HeadRef: row.Head.Ref, HeadSHA: row.Head.SHA,
			BaseRef: row.Base.Ref, Fork: row.Head.Repo == nil || row.Head.Repo.ID != row.Base.Repo.ID, Commits: []string{}, UpdatedAt: updated}
		switch {
		case row.MergedAt != nil && row.State == domain.RemotePullClosed:
			pull.State, pull.MergeSHA = domain.RemotePullMerged, row.MergeSHA
		case row.MergedAt != nil, row.State != domain.RemotePullOpen && row.State != domain.RemotePullClosed:
			return nil, false, malformedRemoteGit("pull request")
		}
		if pull.Validate() != nil {
			return nil, false, malformedRemoteGit("pull request")
		}
		pulls = append(pulls, pull)
	}
	return pulls, len(rows) == remoteGitPageSize, nil
}
