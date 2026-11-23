package store

import (
	"context"
)

type graphSourceCountingContext struct {
	context.Context
	checks int
}

func (ctx *graphSourceCountingContext) Err() error {
	ctx.checks++
	return ctx.Context.Err()
}
