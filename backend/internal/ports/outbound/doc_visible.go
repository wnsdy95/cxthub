package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocVisiblePreparationStore interface {
	HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
	EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}
