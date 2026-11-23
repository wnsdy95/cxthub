//go:build postgres

package app

import (
	"context"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pausedQueryStore struct {
	*store.PostgresStore
	started, release chan struct{}
	once             sync.Once
}

func (s *pausedQueryStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	r, err := s.PostgresStore.GetRepo(ctx, id)
	s.once.Do(func() {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	return r, err
}
