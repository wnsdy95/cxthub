package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepositoryBindings keeps the one-to-one product/sync identity mapping explicit.
// Resolving an alias never bypasses the caller's repository authorization.
type RepositoryBindings interface {
	GetBoundRepo(context.Context, string) (domain.Repo, error)
	InviteTargets(context.Context, string) ([]string, error)
}
