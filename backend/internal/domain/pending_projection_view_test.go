package domain

import (
	"context"
)

type pendingProjectionCancelContext struct {
	context.Context
	remaining int
}

func (ctx *pendingProjectionCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}
