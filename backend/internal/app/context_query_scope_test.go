package app

import (
	"context"
)

type contextBorrowedStore struct {
	*repositoryScopeReadStore
}

func (storage *contextBorrowedStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(repositoryScopeReadKey{}) == storage.repositoryScopeReadStore
}
