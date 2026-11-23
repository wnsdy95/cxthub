package domain_test

import (
	"context"
)

type projectionReviewCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *projectionReviewCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
