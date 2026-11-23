package domain

import (
	"context"
)

type legacyCanonicalNumberRegistryCommitContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalNumberRegistryCommitContext) Err() error {
	ctx.checks++
	if ctx.checks == 2 {
		return context.Canceled
	}
	return ctx.Context.Err()
}
