package domain

import (
	"context"
)

type legacyNativeCanonicalOwnerContext struct {
	context.Context
	calls int
	limit int
}

func (ctx *legacyNativeCanonicalOwnerContext) Err() error {
	ctx.calls++
	if ctx.calls > ctx.limit {
		return context.Canceled
	}
	return ctx.Context.Err()
}
