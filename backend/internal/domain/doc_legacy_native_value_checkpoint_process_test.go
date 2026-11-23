package domain

import (
	"context"
)

type legacyNativeValueCheckpointW76CancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (current *legacyNativeValueCheckpointW76CancelContext) Err() error {
	current.checks++
	if current.checks == 2 {
		current.cancel()
	}
	return current.Context.Err()
}
