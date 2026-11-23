//go:build postgres

package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryStateAuthorizationPGStore struct {
	*store.PostgresStore
	before  func(context.Context) error
	scopes  int
	payload int
}

func (storage *repositoryStateAuthorizationPGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if before := storage.before; before != nil {
		storage.before = nil
		if err := before(ctx); err != nil {
			return err
		}
	}
	return storage.PostgresStore.WithinReadSnapshot(ctx, func(bound context.Context) error {
		storage.scopes++
		if !storage.InReadOnlyTransaction(bound) || inbound.DocumentSnapshotReadAuthorizer(bound) == nil {
			return domain.ErrIntegrity
		}
		return read(bound)
	})
}
