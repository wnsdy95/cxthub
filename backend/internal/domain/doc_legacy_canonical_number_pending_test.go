package domain

import (
	"context"
)

type legacyCanonicalNumberPendingPreparationContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalNumberPendingPreparationContext) Err() error {
	ctx.checks++
	return ctx.Context.Err()
}
