package gitctx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

const (
	githubAPIBase       = "https://api.github.com"
	githubAPIVersion    = "2022-11-28"
	githubPRPageLimit   = 10
	githubResolveWindow = 5 * time.Second
)

// GitHubPRMergeResolver discovers merged GitHub pull requests represented by
// commits newly introduced into the checked-out base branch.
type GitHubPRMergeResolver struct {
	client  *http.Client
	apiBase string
	token   func() string
}

// NewGitHubPRMergeResolver creates the production GitHub discovery adapter.
// Public repositories work without a token; private repositories can use an
// explicitly supplied CXT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN.
func NewGitHubPRMergeResolver() *GitHubPRMergeResolver {
	return newGitHubPRMergeResolver(
		&http.Client{Timeout: githubResolveWindow},
		githubAPIBase,
		cachedToken(githubToken),
	)
}

func cachedToken(resolve func() string) func() string {
	var once sync.Once
	var token string
	return func() string {
		once.Do(func() { token = resolve() })
		return token
	}
}

func newGitHubPRMergeResolver(client *http.Client, apiBase string, token func() string) *GitHubPRMergeResolver {
	return &GitHubPRMergeResolver{
		client:  client,
		apiBase: strings.TrimRight(apiBase, "/"),
		token:   token,
	}
}

func githubToken() string {
	return discoverGitHubToken(os.Getenv, githubCLIToken)
}

func discoverGitHubToken(getenv func(string) string, cliToken func() string) string {
	for _, name := range []string{"CXT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(getenv(name)); token != "" {
			return token
		}
	}
	if cliToken != nil {
		return strings.TrimSpace(cliToken())
	}
	return ""
}

// githubCLIToken reuses the user's existing `gh auth login` credential when
// no explicit environment token is configured. gh may keep the token in the
// OS keyring (not ~/.config/gh), so invoking its non-interactive token command
// is the only portable discovery path. Keep this bounded inside Git hooks.
func githubCLIToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

type githubPullRequest struct {
	Number         int     `json:"number"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Base           struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// ResolveMergedPullRequests lists recently closed PRs for the base branch and
// matches GitHub's post-merge merge_commit_sha against incoming Git commits.
// GitHub defines that field for merge, squash, and rebase merge methods.
func (r *GitHubPRMergeResolver) ResolveMergedPullRequests(
	ctx context.Context,
	gitRemoteURL, baseBranch string,
	commitSHAs []string,
) ([]outbound.MergedPullRequest, error) {
	pulls, next, err := r.resolveMergedPullRequests(ctx, gitRemoteURL, baseBranch, commitSHAs, 1, githubPRPageLimit, false)
	if err == nil && next != 0 {
		return nil, fmt.Errorf("github PR resolver: provider history pagination is incomplete")
	}
	return pulls, err
}

func (r *GitHubPRMergeResolver) ResolveMergedPullRequestsPage(ctx context.Context, gitRemoteURL, baseBranch string, commitSHAs []string, page int) ([]outbound.MergedPullRequest, int, error) {
	if page < 1 {
		return nil, 0, fmt.Errorf("github PR resolver: invalid provider page")
	}
	return r.resolveMergedPullRequests(ctx, gitRemoteURL, baseBranch, commitSHAs, page, 1, true)
}

func (r *GitHubPRMergeResolver) resolveMergedPullRequests(ctx context.Context, gitRemoteURL, baseBranch string, commitSHAs []string, firstPage, pageCount int, stable bool) ([]outbound.MergedPullRequest, int, error) {
	owner, repo, ok := githubRepository(gitRemoteURL)
	if !ok || baseBranch == "" || len(commitSHAs) == 0 {
		return nil, 0, nil
	}

	positions := make(map[string]int, len(commitSHAs))
	for i, sha := range commitSHAs {
		sha = strings.ToLower(strings.TrimSpace(sha))
		if sha != "" {
			positions[sha] = i
		}
	}
	if len(positions) == 0 {
		return nil, 0, nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, githubResolveWindow)
	defer cancel()

	repoFullName := strings.ToLower(owner + "/" + repo)
	found := make(map[int]outbound.MergedPullRequest)
	next := firstPage
	for count := 0; count < pageCount; count++ {
		page := next
		endpoint, err := url.Parse(r.apiBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls")
		if err != nil {
			return nil, 0, fmt.Errorf("github PR resolver: build endpoint: %w", err)
		}
		query := endpoint.Query()
		query.Set("state", "closed")
		query.Set("base", baseBranch)
		query.Set("sort", "updated")
		query.Set("direction", "desc")
		if stable {
			query.Set("state", "all")
			query.Del("base")
			query.Set("sort", "created")
			query.Set("direction", "asc")
		}
		query.Set("per_page", "100")
		query.Set("page", fmt.Sprintf("%d", page))
		endpoint.RawQuery = query.Encode()

		var pulls []githubPullRequest
		if err := r.getJSON(resolveCtx, endpoint.String(), &pulls); err != nil {
			return nil, 0, err
		}
		if pulls == nil || len(pulls) > 100 {
			return nil, 0, fmt.Errorf("github PR resolver: invalid provider page")
		}
		for _, pull := range pulls {
			sha := strings.ToLower(strings.TrimSpace(pull.MergeCommitSHA))
			if pull.MergedAt == nil || pull.Number <= 0 || pull.Base.Ref != baseBranch || pull.Head.Ref == "" {
				continue
			}
			if _, wanted := positions[sha]; !wanted {
				continue
			}
			// A fork PR cannot safely address a branch ref in the base cxt
			// repository. Skipping also prevents same-name branch confusion.
			if pull.Base.Repo == nil || pull.Head.Repo == nil ||
				strings.ToLower(pull.Base.Repo.FullName) != repoFullName ||
				strings.ToLower(pull.Head.Repo.FullName) != repoFullName {
				continue
			}
			found[pull.Number] = outbound.MergedPullRequest{
				Number:         pull.Number,
				BaseBranch:     pull.Base.Ref,
				HeadBranch:     pull.Head.Ref,
				MergeCommitSHA: sha,
				HeadSHA:        pull.Head.SHA,
			}
		}
		if len(pulls) < 100 {
			next = 0
			break
		}
		next = page + 1
		if next <= page {
			return nil, 0, fmt.Errorf("github PR resolver: invalid provider cursor")
		}
	}

	out := make([]outbound.MergedPullRequest, 0, len(found))
	for _, pull := range found {
		out = append(out, pull)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := positions[out[i].MergeCommitSHA], positions[out[j].MergeCommitSHA]
		if left != right {
			return left < right
		}
		return out[i].Number < out[j].Number
	})
	return out, next, nil
}

func (r *GitHubPRMergeResolver) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("github PR resolver: request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "cxt")
	if r.token != nil {
		if token := strings.TrimSpace(r.token()); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("github PR resolver: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		remaining := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining"))
		if remaining != "" {
			return fmt.Errorf("github PR resolver: GitHub API returned %s (rate-limit remaining=%s)", resp.Status, remaining)
		}
		return fmt.Errorf("github PR resolver: GitHub API returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("github PR resolver: decode response: %w", err)
	}
	return nil
}

func githubRepository(rawRemote string) (owner, repo string, ok bool) {
	normalized := NormalizeRemoteURL(rawRemote)
	const prefix = "github.com/"
	if !strings.HasPrefix(normalized, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(normalized, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", "", false
	}
	return parts[0], parts[1], true
}

var _ outbound.PullRequestMergeResolver = (*GitHubPRMergeResolver)(nil)
