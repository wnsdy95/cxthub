package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func withRetainedObjects[T any](ctx context.Context, store outbound.SessionStore, fn func() (T, error)) (out T, err error) {
	retention, ok := store.(outbound.ObjectRetention)
	if !ok {
		return fn()
	} // Stores without collection need no retention lease.
	err = retention.WithObjectsRetained(ctx, func() error { out, err = fn(); return err })
	return
}
