package domain

import (
	"context"
)

type docToolDisplayCancelContext struct {
	context.Context
	remaining int
}

func (ctx *docToolDisplayCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}
