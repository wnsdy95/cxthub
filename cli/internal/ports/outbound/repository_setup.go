package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SetupMarker writes the mark that cxt was set up in a repository and returns
// the root that holds it. An existing mark is left as it is.
type SetupMarker interface {
	MarkSetUp(ctx context.Context, cwd string) (string, error)
}

// RepoRegistry registers a repository with its server and returns the
// server's record of it.
type RepoRegistry interface {
	RegisterRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error)
}
