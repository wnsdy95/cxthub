package domain

import (
	"context"
)

type docFindCoordinatesCancelContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *docFindCoordinatesCancelContext) Err() error {
	ctx.checks++
	if ctx.cancelAt > 0 && ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
