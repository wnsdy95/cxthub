package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A transactional adapter which has no positive transaction-mode capability.
type unknownDeliveryTransactionStore struct{ *store.FSStore }

func (s *unknownDeliveryTransactionStore) WithinRepository(ctx context.Context, _ domain.ContentHash, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *unknownDeliveryTransactionStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
