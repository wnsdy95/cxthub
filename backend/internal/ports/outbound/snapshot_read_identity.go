package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type CachedIdentityVerifier interface {
	VerifyCached(context.Context, string) (domain.User, error)
}

type BreakGlassGrantReader interface {
	GetBreakGlassGrant(context.Context, string, string, string, string) (domain.BreakGlassGrant, error)
}
