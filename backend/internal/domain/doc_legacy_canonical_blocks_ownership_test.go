package domain

import (
	"context"
)

type legacyCanonicalBlocksOwnershipCancellationContext struct {
	context.Context
	cancelWhen func() bool
}

func (ctx legacyCanonicalBlocksOwnershipCancellationContext) Err() error {
	if ctx.cancelWhen() {
		return context.Canceled
	}
	return ctx.Context.Err()
}
