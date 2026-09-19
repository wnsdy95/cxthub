package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// DocJobStore owns both job state and verified body publication. Complete must
// fence the lease before any write and commit body + receipt atomically in the
// production adapter. No snapshot, memory attachment or ref is changed here.
type DocJobStore interface {
	EnqueueDocJob(context.Context, domain.DocFinalizationJob) (domain.DocFinalizationJob, error)
	GetDocJob(context.Context, domain.ContentHash, string) (domain.DocFinalizationJob, error)
	ClaimDocJob(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.DocFinalizationJob, error)
	RenewDocJob(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error
	FinishDocJob(context.Context, domain.DocFinalizationJob, time.Time) error
	CompleteDocJob(context.Context, domain.DocFinalizationJob, domain.VerifiedSessionDoc, time.Time) error
}
