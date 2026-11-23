//go:build postgres

package app

import (
	"context"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

type graphReadPGTransactions struct {
	*store.PostgresStore
	active atomic.Int64
	reads  atomic.Int64
}

func (storage *graphReadPGTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InReadOnlyTransaction(ctx) {
		return storage.PostgresStore.WithinReadSnapshot(ctx, read)
	}
	storage.active.Add(1)
	storage.reads.Add(1)
	defer storage.active.Add(-1)
	return storage.PostgresStore.WithinReadSnapshot(ctx, read)
}
