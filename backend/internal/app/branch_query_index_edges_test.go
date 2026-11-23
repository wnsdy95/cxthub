package app

import (
	"context"
	"sync/atomic"
)

type branchIndexCancellationContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   atomic.Int32
	cancelAt int32
}

func (ctx *branchIndexCancellationContext) Err() error {
	if ctx.checks.Add(1) == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
