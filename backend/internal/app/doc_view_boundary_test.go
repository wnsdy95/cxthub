package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docViewBoundaryFake struct {
	*docViewCountStore
	readonly, ready bool
	boundary        int
	failure         error
	queued, reads   int
	state, reason   string
}

func (storage *docViewBoundaryFake) InReadOnlyTransaction(context.Context) bool {
	return storage.readonly
}

func (storage *docViewBoundaryFake) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	storage.reads++
	if storage.failure != nil {
		return 0, storage.failure
	}
	if !storage.ready {
		return 0, domain.ErrDocRangeIndexUnavailable
	}
	return storage.boundary, nil
}
