package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Recovery writes and their audit share the identity transaction. No FS fallback.
type OwnerRecoveryStore interface {
	CredentialAssurances
	GetOwnerRecovery(context.Context, string, string) (domain.OwnerRecovery, error)
	PutOwnerRecovery(context.Context, domain.OwnerRecovery) error
	GetOwnerRepairSession(context.Context, string, string) (domain.OwnerRepairSession, error)
	PutOwnerRepairSession(context.Context, domain.OwnerRepairSession) error
}
