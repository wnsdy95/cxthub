package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DomainTXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}

// Domain claims require a shared identity transaction, including audit. A store
// that cannot provide cross-process rollback must not enable this capability.
type EnterpriseDomainStore interface {
	IdentityTransactions
	ListEnterpriseDomains(context.Context, string) ([]domain.EnterpriseDomain, error)
	GetEnterpriseDomain(context.Context, string, string) (domain.EnterpriseDomain, error)
	PutEnterpriseDomain(context.Context, domain.EnterpriseDomain) error
	RemoveEnterpriseDomain(context.Context, string, string) error
	OtherVerifiedEnterpriseDomain(context.Context, string, string, time.Time) (bool, error)
}
