package domain

import (
	"context"
)

type legacyNativeGoGraphSetTrivialBatchContext struct {
	context.Context
	check func() error
}

func (ctx legacyNativeGoGraphSetTrivialBatchContext) Err() error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}
	return ctx.check()
}
