package domain

import (
	"context"
)

type legacyNativeFloat64DecimalTestContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *legacyNativeFloat64DecimalTestContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
