package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// This probe checks the application transaction-context boundary only. It is
// deliberately not a substitute for PostgreSQL rollback/concurrency testing.
type inheritedSelectionTxKey struct{}
type inheritedSelectionTxProbe struct {
	*store.FSStore
	reads                     map[domain.ContentHash]int
	corrupt                   domain.ContentHash
	histories, applies, locks int
}

func (s *inheritedSelectionTxProbe) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, inheritedSelectionTxKey{}, repo))
}
func (s *inheritedSelectionTxProbe) WithinReadSnapshot(context.Context, func(context.Context) error) error {
	return domain.ErrConflict
}
func (s *inheritedSelectionTxProbe) LockRepositoryAccess(ctx context.Context, _ string, _ string) error {
	if ctx.Value(inheritedSelectionTxKey{}) == nil {
		return domain.ErrConflict
	}
	s.locks++
	return nil
}
func (s *inheritedSelectionTxProbe) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	if ctx.Value(inheritedSelectionTxKey{}) != repo {
		return domain.Snapshot{}, domain.ErrConflict
	}
	return s.FSStore.GetSnapshot(ctx, repo, id)
}
