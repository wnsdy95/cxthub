package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RepositoryInitialization creates only a provably new server repository. It
// cannot upgrade an existing legacy repository or fabricate Git history.
type RepositoryInitialization interface {
	RepositoryInitializationState(ctx context.Context, repo, initialBranch string) (RepositoryInitializationState, error)
	ReadRepositoryInitialization(context.Context, string) (domain.RepositoryInitializationReceipt, error)
	BeginRepositoryInitialization(context.Context, domain.Repo) (domain.RepositoryInitializationReceipt, error)
	FinalizeRepositoryInitialization(context.Context, string, domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error)
}

// RepositoryInitializationState is an authorized runtime projection, not local
// configuration and not proof that an empty repository was newly created.
// InitialAnchorAvailable applies to the requested nonempty initialBranch.
type RepositoryInitializationState struct {
	Repo                   domain.Repo
	InitialAnchorAvailable bool
}
