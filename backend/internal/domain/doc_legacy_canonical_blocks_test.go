package domain

import (
	"context"
)

type legacyCanonicalBlocksInspectionContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalBlocksInspectionContext) Err() error {
	ctx.checks++
	if ctx.checks >= 8 {
		return context.Canceled
	}
	return ctx.Context.Err()
}
