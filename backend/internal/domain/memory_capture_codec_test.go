package domain

import (
	"context"
)

type memoryCaptureTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *memoryCaptureTestContext) Err() error {
	ctx.checks++
	if ctx.cancelAfter > 0 && ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}
