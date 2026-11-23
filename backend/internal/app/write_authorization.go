package app

import (
	"context"
)

type writeActionKey struct{}

func writeAction(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, writeActionKey{}, action)
}
