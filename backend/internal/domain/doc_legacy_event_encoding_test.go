package domain

import (
	"context"
)

type legacyEventEncodingFinalizationContext struct {
	context.Context
	completed func() bool
}

func (ctx legacyEventEncodingFinalizationContext) Err() error {
	if ctx.completed() {
		return context.Canceled
	}
	return ctx.Context.Err()
}
