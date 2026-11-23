package domain

import (
	"context"
)

type legacyCheckpointOwnerConstructionContext struct {
	context.Context
	inspect func()
}

func (current *legacyCheckpointOwnerConstructionContext) Err() error {
	current.inspect()
	return current.Context.Err()
}
