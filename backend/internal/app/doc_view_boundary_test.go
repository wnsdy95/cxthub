package app

import (
	"context"
	"errors"
	"testing"
	"time"

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

func (storage *docViewBoundaryFake) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.queued++
	return domain.DocRangeIndexJob{State: storage.state, Reason: storage.reason}, nil
}

func TestDocViewBoundaryNeverFallsBackAfterGenerationFailure(t *testing.T) {
	ctx := context.Background()
	for _, readonly := range []bool{true, false} {
		for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrDocRangeIndexUnavailable, context.Canceled} {
			storage := &docViewBoundaryFake{readonly: readonly, failure: failure}
			service := NewService(nil, storage, nil, nil)
			source := docViewSource{root: true, total: 128, block: func(int) (domain.ContentHash, error) {
				t.Fatal("persisted boundary failure fell back to prefix traversal")
				return "", nil
			}}
			_, err := service.docViewBoundary(ctx, h('1'), h('2'), h('3'), source, source)
			if readonly {
				if !errors.Is(err, failure) || storage.reads != 1 {
					t.Fatal("boundary failure changed", err, storage.reads)
				}
			} else if !errors.Is(err, domain.ErrConflict) || storage.reads != 0 {
				t.Fatal("unbound boundary lookup", err, storage.reads)
			}
		}
	}
	for _, boundary := range []int{-1, 129} {
		storage := &docViewBoundaryFake{readonly: true, ready: true, boundary: boundary}
		service := NewService(nil, storage, nil, nil)
		if _, err := service.docViewBoundary(ctx, h('1'), h('2'), h('3'), docViewSource{root: true, total: 128}, docViewSource{root: true, total: 200}); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("invalid persisted boundary accepted", boundary, err)
		}
	}
}
