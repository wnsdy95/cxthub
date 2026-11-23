package domain

import (
	"context"
)

type graphProjectionTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *graphProjectionTestContext) Err() error {
	ctx.checks++
	if ctx.cancelAfter != 0 && ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}
