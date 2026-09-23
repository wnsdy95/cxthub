package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepositoryOrganizationAccess reads current facts in the caller's transaction.
// The domain, not the adapter, decides which role these facts confer.
type RepositoryOrganizationAccess interface {
	RepositoryOrganizationAccess(context.Context, string, string) (domain.OrganizationRepositoryAccess, error)
}
