package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocRangeStore interface {
	DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error)
	DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error)
	EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error
}
