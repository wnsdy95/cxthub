package domain

import (
	"context"
)

type legacyNativeGoGraphW85CutContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	limit  int
}

func (current *legacyNativeGoGraphW85CutContext) Err() error {
	current.calls++
	if current.calls >= current.limit {
		current.cancel()
	}
	return current.Context.Err()
}
