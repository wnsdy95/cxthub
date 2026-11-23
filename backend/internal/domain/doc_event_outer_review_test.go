package domain

import (
	"context"
)

type outerReviewCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *outerReviewCancelContext) Err() error {
	ctx.checks++
	if ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
