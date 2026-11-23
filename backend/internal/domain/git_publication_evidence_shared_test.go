package domain

import (
	"context"
)

type gitPublicationIndexCancelContext struct {
	context.Context
	cancel context.CancelFunc
	after  int
	calls  int
}

func (ctx *gitPublicationIndexCancelContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.after {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
