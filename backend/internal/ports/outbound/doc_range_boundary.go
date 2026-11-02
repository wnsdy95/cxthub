package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocRangeBoundaryStore interface {
	RepositoryTransactionState
	DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error)
	EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}
