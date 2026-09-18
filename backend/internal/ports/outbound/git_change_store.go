package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

// GitChangeStore persists requests and terminal evidence in the same row. All
// transitions are atomic and fenced. Network reads happen outside transactions.
type GitChangeStore interface {
	EnqueueGitChange(context.Context, domain.GitChangeJob) (domain.GitChangeJob, error)
	GetGitChange(context.Context, domain.ContentHash, string) (domain.GitChangeJob, error)
	ListGitChanges(context.Context, domain.ContentHash, string, int) ([]domain.GitChangeSummary, error)
	ClaimGitChange(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitChangeJob, error)
	FinishGitChange(context.Context, domain.GitChangeJob) error
	RetryGitChange(context.Context, domain.ContentHash, string, time.Time) error
}
