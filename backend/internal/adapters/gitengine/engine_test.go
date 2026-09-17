package gitengine

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
)

type failedRead struct {
	outbound.MetadataStore
	err error
}

func (s failedRead) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{}, s.err
}

func TestGraphReadsFailClosed(t *testing.T) {
	for _, failure := range []error{errors.New("database unavailable"), domain.ErrNotFound, context.Canceled} {
		e := NewEngine(failedRead{err: failure})
		ctx := context.Background()
		if _, err := e.IsAncestor(ctx, "repo", "a", "b"); !errors.Is(err, failure) {
			t.Fatalf("IsAncestor swallowed %v: %v", failure, err)
		}
		if _, err := e.MergeBase(ctx, "repo", "a", "b"); !errors.Is(err, failure) {
			t.Fatalf("MergeBase swallowed %v: %v", failure, err)
		}
		if _, err := e.AncestorsClosure(ctx, "repo", []domain.ContentHash{"a"}); !errors.Is(err, failure) {
			t.Fatalf("closure swallowed %v: %v", failure, err)
		}
		if _, err := e.ClassifyRefMove(ctx, "repo", "a", "b"); !errors.Is(err, failure) {
			t.Fatalf("classification swallowed %v: %v", failure, err)
		}
	}
}
