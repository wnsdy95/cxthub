package domain

import (
	"context"
)

type legacyNativeFloat64RetainedCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *legacyNativeFloat64RetainedCancelContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
