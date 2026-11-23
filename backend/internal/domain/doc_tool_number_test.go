package domain

import (
	"context"
)

type docToolNumberLateCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	at     int
}

func (ctx *docToolNumberLateCancelContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.at {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
