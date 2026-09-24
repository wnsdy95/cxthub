package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Mutations participate in IdentityTransactions. Identity and installation
// uniqueness is also enforced by storage, not just application preflight.
type GitHubStore interface {
	GetGitHubConnection(context.Context, string) (domain.GitHubConnection, error)
	ListGitHubConnections(context.Context) ([]domain.GitHubConnection, error)
	PutGitHubConnection(context.Context, domain.GitHubConnection) error
	PutGitHubIdentity(context.Context, domain.GitHubIdentity) error
	ListGitHubIdentities(context.Context) ([]domain.GitHubIdentity, error)
	GetGitHubRequest(context.Context, string) (domain.GitHubConnectRequest, error)
	PutGitHubRequest(context.Context, domain.GitHubConnectRequest) error
	GetGitHubDelivery(context.Context, string) (domain.GitHubDelivery, error)
	PutGitHubDelivery(context.Context, domain.GitHubDelivery) error
	ListGitHubDeliveries(context.Context) ([]domain.GitHubDelivery, error)
	ReplaceGitHubTeamMembers(context.Context, string, []domain.GitHubTeamMember) error
}

type GitHubAuthorization struct {
	Identity     domain.GitHubIdentity
	Installation domain.GitHubInstallation
}

// No access token is returned to application callers or persisted as profile
// data. The adapter exchanges and discards user tokens within authorization.
type GitHubApp interface {
	InstallURL(string) string
	AuthorizeURL(string, string) string
	Authorize(context.Context, string, string, int64) (GitHubAuthorization, error)
	Installation(context.Context, int64) (domain.GitHubInstallation, error)
	Repositories(context.Context, int64) ([]domain.GitHubRepository, error)
	Teams(context.Context, int64, string) ([]domain.GitHubTeam, error)
	TeamMembers(context.Context, int64, string, string) ([]int64, error)
	MergedPullRequests(context.Context, int64, domain.GitHubRepository, int) ([]domain.PullRequestMerge, bool, error)
	Invalidate(int64)
}

// GitRepositoryScope is set by application use-cases, never read from headers.
// It selects installation credentials without relying on mutable origin names.
type gitRepositoryScope struct{}

func WithGitRepository(ctx context.Context, id domain.ContentHash) context.Context {
	return context.WithValue(ctx, gitRepositoryScope{}, id)
}
func GitRepository(ctx context.Context) domain.ContentHash {
	id, _ := ctx.Value(gitRepositoryScope{}).(domain.ContentHash)
	return id
}
