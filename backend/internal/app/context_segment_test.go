package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type segmentReadSpy struct {
	*store.FSStore
	reads        int
	catalogReads int
}

func (s *segmentReadSpy) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	s.catalogReads++
	return s.FSStore.ListSnapshots(ctx, repo, branch)
}
func (s *segmentReadSpy) DocReadIndex(ctx context.Context, r, h domain.ContentHash) (domain.DocReadIndex, error) {
	s.reads++
	return s.FSStore.DocReadIndex(ctx, r, h)
}
