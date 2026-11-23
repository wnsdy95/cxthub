package domain

import (
	"context"
)

type memoryReadTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *memoryReadTestContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}
