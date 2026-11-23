package domain

import (
	"context"
)

type legacyNativeScalarCheckpointProcessStore struct {
	parts    [][]byte
	width    int
	reads    [][2]int
	contexts []context.Context
	borrowed []byte
	failure  error
	delta    int
	cancel   context.CancelFunc
}

func (storage *legacyNativeScalarCheckpointProcessStore) read(ctx context.Context, offset, length int) ([]byte, error) {
	storage.reads = append(storage.reads, [2]int{offset, length})
	storage.contexts = append(storage.contexts, ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storage.failure != nil {
		return nil, storage.failure
	}
	if offset < 0 || storage.width <= 0 || offset%storage.width != 0 || offset/storage.width >= len(storage.parts) {
		return nil, ErrIntegrity
	}
	body := storage.parts[offset/storage.width]
	if length != len(body) || length+storage.delta < 0 {
		return nil, ErrIntegrity
	}
	storage.borrowed = make([]byte, length+storage.delta)
	copy(storage.borrowed, body)
	if storage.cancel != nil {
		storage.cancel()
	}
	return storage.borrowed, nil
}
