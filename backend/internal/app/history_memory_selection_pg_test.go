//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyMemorySelectionTxKey struct{}
type historyMemorySelectionTxPG struct {
	*store.PostgresStore
	histories, memories, applies int
	failAfterApply               bool
}

func (s *historyMemorySelectionTxPG) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	return s.PostgresStore.WithinRepository(ctx, repo, func(bound context.Context) error {
		return fn(context.WithValue(bound, historyMemorySelectionTxKey{}, s))
	})
}
