package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocRangeReadinessStore interface {
	HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
}

type DocByteRangePreparationStore interface {
	LegacyByteRangePublicationEnabled() bool
	HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
	EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type DocRangeJobStore interface {
	EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
	ClaimDocRangeJob(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error)
	RenewDocRangeJob(context.Context, domain.DocRangeIndexJob, time.Time, time.Duration) error
	BuildDocRangeJob(context.Context, domain.DocRangeIndexJob) error
	FinishDocRangeJob(context.Context, domain.DocRangeIndexJob, time.Time) error
}
