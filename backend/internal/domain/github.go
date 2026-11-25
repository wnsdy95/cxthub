package domain

import "time"

// GitHub records use immutable numeric IDs. Names are display/routing metadata,
// never identity or evidence of CXTHub membership.
type GitHubIdentity struct {
	UserID     string `json:"user_id"`
	ExternalID int64  `json:"external_id"`
	Login      string `json:"login"`
}
type GitHubInstallation struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Login     string `json:"login"`
	Kind      string `json:"kind"`
	Suspended bool   `json:"suspended"`
}
type GitHubRepository struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	FullName  string `json:"full_name"`
	Private   bool   `json:"private"`
}
type GitHubBinding struct {
	GitOrigin     string      `json:"git_origin"`
	RepositoryID  string      `json:"repository_id"`
	ContextRepoID ContentHash `json:"context_repo_id"`
	ExternalID    int64       `json:"external_id"`
}
type GitHubTeam struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}
type GitHubTeamMapping struct {
	TeamID      string `json:"team_id"`
	ExternalID  int64  `json:"external_id"`
	SyncMembers bool   `json:"sync_members"`
}
type GitHubConnection struct {
	PRPages           map[int64]int       `json:"pr_pages"`
	UnresolvedMembers int                 `json:"unresolved_members"`
	NamespaceID       string              `json:"namespace_id"`
	Installation      GitHubInstallation  `json:"installation"`
	Generation        int64               `json:"generation"`
	Enabled           bool                `json:"enabled"`
	Status            string              `json:"status"`
	Repositories      []GitHubRepository  `json:"repositories"`
	Bindings          []GitHubBinding     `json:"bindings"`
	Teams             []GitHubTeam        `json:"teams"`
	Mappings          []GitHubTeamMapping `json:"mappings"`
	UpdatedBy         string              `json:"updated_by"`
	CheckedAt         time.Time           `json:"checked_at"`
	NextSync          time.Time           `json:"next_sync"`
}

// GitHubRepositoryState says whether the server reads a repository on GitHub
// through an installation of the GitHub App.
type GitHubRepositoryState string

const (
	// GitHubRepositoryConnected: the installation of the repository's owner
	// is in use, and it was told to read this repository.
	GitHubRepositoryConnected GitHubRepositoryState = "connected"
	// GitHubRepositoryNotConnected: the owner has no installation in use, or
	// the installation was not told to read this repository.
	GitHubRepositoryNotConnected GitHubRepositoryState = "not_connected"
	// GitHubAppNotConfigured: this server has no GitHub App to connect with.
	GitHubAppNotConfigured GitHubRepositoryState = "not_configured"
)

// GitHubRepositoryConnection is what a reader of a repository is told about
// its connection with GitHub. ConnectURL is the page where an owner of the
// repository connects it; a server without a GitHub App has no such page.
type GitHubRepositoryConnection struct {
	State      GitHubRepositoryState `json:"state"`
	ConnectURL string                `json:"connect_url,omitempty"`
}

// These records never cross the HTTP boundary. PKCE/state are server-side,
// short-lived and consumed before the authorization code is exchanged.
type GitHubConnectRequest struct {
	Hash           string
	ActorID        string
	NamespaceID    string
	InstallationID int64
	Verifier       string
	Generation     int64
	ExpiresAt      time.Time
	Consumed       bool
}
type GitHubDelivery struct {
	BodyHash    string
	ID          string
	Kind        string
	Body        []byte
	Done        bool
	Attempts    int
	NextAttempt time.Time
	Lease       string
	LeaseUntil  time.Time
}

// Independently sourced memberships expire if synchronization stops. Manual
// team membership is not overwritten or removed by these grants.
type GitHubTeamMember struct {
	NamespaceID string    `json:"namespace_id"`
	TeamID      string    `json:"team_id"`
	UserID      string    `json:"user_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}
