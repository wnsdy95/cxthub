package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// OAuthGrants persists per-authorization identity and hashed replay evidence.
// Mutations must share IdentityTransactions with sessions and account audit.
type OAuthGrants interface {
	CreateOAuthGrant(context.Context, domain.OAuthGrant) error
	GetOAuthGrant(context.Context, string) (domain.OAuthGrant, error)
	UpdateOAuthGrant(context.Context, domain.OAuthGrant) error
	CreateOAuthRefreshReference(context.Context, domain.OAuthRefreshReference) error
	GetOAuthRefreshReference(context.Context, string) (domain.OAuthRefreshReference, error)
}
