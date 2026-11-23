package domain

import (
	"context"
	"sync"
)

type docVisibleReaderWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *docVisibleReaderWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
