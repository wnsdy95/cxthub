package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocFindFieldsStore interface {
	ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error)
	EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
	ClaimDocFindFields(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error)
	RenewDocFindFields(context.Context, domain.DocRangeIndexJob, time.Time, time.Duration) error
	PutDocFindFields(context.Context, domain.DocRangeIndexJob, domain.DocFindFields) error
	FinishDocFindFields(context.Context, domain.DocRangeIndexJob, time.Time) error
}
