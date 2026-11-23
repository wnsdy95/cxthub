package domain

import (
	"context"
)

type legacyNativeValueWireOracleCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	after  int
}

func (current *legacyNativeValueWireOracleCancelContext) Err() error {
	current.calls++
	if current.calls >= current.after {
		current.cancel()
	}
	return current.Context.Err()
}
