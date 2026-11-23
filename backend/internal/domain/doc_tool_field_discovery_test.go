package domain

import (
	"context"
)

type docToolFieldCancelContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *docToolFieldCancelContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
