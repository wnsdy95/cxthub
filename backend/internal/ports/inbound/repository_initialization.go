package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryInitialization interface {
	GetRepositoryInitialization(context.Context, domain.ContentHash) (domain.RepositoryInitializationReceipt, error)
	BeginRepositoryInitialization(context.Context, string, domain.ContentHash, domain.RepositoryInitializationRequest) (domain.RepositoryInitializationReceipt, error)
	FinalizeRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error)
}

// Read-only metadata for setup/Connect. A reader never needs manage permission
// merely to discover whether the optional initial legacy anchor is available.
type RepositoryInitializationQuery interface {
	GetRepositoryInitializationView(context.Context, domain.ContentHash, string) (domain.Repo, bool, error)
}
