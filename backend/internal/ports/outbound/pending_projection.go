package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PendingProjectionStore interface {
	ConfigurePendingProjection(context.Context, domain.ContentHash, bool) error
	ClaimPendingProjection(context.Context, time.Duration) (domain.PendingProjectionLease, error)
	RenewPendingProjection(context.Context, domain.PendingProjectionLease, time.Duration) error
	SavePendingProjectionPage(context.Context, domain.PendingProjectionLease, domain.RepositoryViewSourcePage) error
	LoadPendingProjectionInputs(context.Context, domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error)
	PublishPendingProjection(context.Context, domain.PendingProjectionLease, []domain.PendingProjectionPart) error
	FailPendingProjection(context.Context, domain.PendingProjectionLease, string, bool) error
	ReadPendingProjection(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error)
}
