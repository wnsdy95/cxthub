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
	PrepareDocJob(context.Context, domain.VerifiedSessionDoc) (PreparedDocPublication, error)
	CompleteDocJob(context.Context, domain.DocFinalizationJob, domain.VerifiedSessionDoc, time.Time) error
}

// PreparedDocPublication binds a verified immutable document to one storage
// adapter's derived write plan. Preparation performs no writes or ownership
// grants and runs while the worker can still renew its lease. Complete must
// recheck the current job fence and atomically publish its body and receipt.
// Complete may first stage invisible rebuildable derivatives under a durable
// fenced job pin; it must not grant document access or complete the receipt then.
// Plans are request-local capabilities, never accepted from a wire format.
type PreparedDocPublication interface {
	Complete(context.Context, domain.DocFinalizationJob, time.Time) error
}
