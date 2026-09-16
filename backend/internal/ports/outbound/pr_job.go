package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// PRJobStore persists acceptance before source lookup. Claim and Finish are atomic.
type PRJobStore interface {
	EnqueuePRJob(context.Context, domain.PRPromotionJob) (domain.PRPromotionJob, error)
	GetPRJob(context.Context, domain.ContentHash, string) (domain.PRPromotionJob, error)
	ListPRJobs(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error)
	// Empty repo/id claims the oldest due job across repositories. ErrNotFound means no work.
	ClaimPRJob(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.PRPromotionJob, error)
	FinishPRJob(context.Context, domain.PRPromotionJob) error
	RetryPRJob(context.Context, domain.ContentHash, string, time.Time) error
}
