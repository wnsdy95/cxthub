package app

import (
	"context"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphReadTransactionKey struct{}

type graphReadTransactions struct {
	*store.FSStore
	active atomic.Int64
	reads  atomic.Int64
}

func (storage *graphReadTransactions) InTransaction(ctx context.Context) bool {
	return ctx.Value(graphReadTransactionKey{}) != nil
}

func (storage *graphReadTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

func (storage *graphReadTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if ctx.Value(graphReadTransactionKey{}) != nil {
		return read(ctx)
	}
	storage.active.Add(1)
	storage.reads.Add(1)
	defer storage.active.Add(-1)
	return read(context.WithValue(ctx, graphReadTransactionKey{}, true))
}
