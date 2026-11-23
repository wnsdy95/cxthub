package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// RepositoryConnector registers the repository of a folder with its server.
type RepositoryConnector interface {
	// Connect registers the repository and returns the server's record of it.
	Connect(ctx context.Context, cwd string) (ConnectOutput, error)
}

// ConnectOutput is the result of RepositoryConnector.Connect.
type ConnectOutput struct {
	// Repo is a server-confirmed record. Binds to repository if RepositoryID != "" (web display).
	Repo domain.Repo
}

// InitRepo is a use-case port for registering the current repo and creating a local .cxt/ store (compatibility rules).
// Called by CLI cxt setup/init/repo create.
type InitRepo interface {
	// Init takes an InitInput and registers the repo and creates a .cxt/ store.
	Init(ctx context.Context, in InitInput) (InitOutput, error)
}

// --- DTOs (domain model comments detailed DTO. Field names/types are contracts) ---

// BranchSummaries gives a session the most recent summary along the branch
// its folder is on, once for each branch the session comes to. emit writes
// the text to the session's tool; a summary is counted as given only after
// emit returned without an error.
type BranchSummaries interface {
	DeliverBranchSummary(ctx context.Context, in BranchSummaryInput, emit func(text string) error) error
}

type BranchSummaryInput struct {
	Cwd       string
	Provider  domain.ProviderKind
	SessionID string
}

// InitInput is the DTO for InitRepo.Init (compatibility rules).
type InitInput struct {
	// Cwd is the target working directory for registration.
	Cwd string
	// RemoteURL is the explicit git remote URL. "" means auto-detect origin (fallback to cwd if not found).
	RemoteURL string
}

// InitOutput is the DTO output of InitRepo.Init (compatibility rules).
type InitOutput struct {
	// RepoID is the ID of the registered repo (normalized remote URL or ContentHash of cwd).
	RepoID string
	// LocalStorePath is the local .cxt/ store path created.
	LocalStorePath string
}
