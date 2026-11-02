package domain

import (
	"context"
	"errors"
)

type GraphProjectionCorruption struct {
	RepoID       ContentHash
	Dataset      string
	Applied      RepositoryViewSourceCheckpoint
	Rules        int
	Serving      bool
	ServingFence int64
	Cause        error
}

func (failure *GraphProjectionCorruption) Error() string {
	return "completed graph projection failed integrity validation"
}

func (failure *GraphProjectionCorruption) Unwrap() error {
	return failure.Cause
}

func (failure GraphProjectionCorruption) Validate() error {
	if failure.Serving && failure.ServingFence <= 0 || !failure.Serving && failure.ServingFence != 0 {
		return ErrValidation
	}
	if failure.Applied.Validate() != nil || failure.Applied.Scope != RepositoryGraphSourceScope ||
		failure.Applied.RepoID != failure.RepoID || failure.Rules != GraphViewProjectionRules {
		return ErrValidation
	}
	dataset := failure.Applied
	dataset.Epoch = failure.Dataset
	if dataset.Validate() != nil {
		return ErrValidation
	}
	if errors.Is(failure.Cause, context.Canceled) || errors.Is(failure.Cause, context.DeadlineExceeded) {
		return ErrValidation
	}
	if !errors.Is(failure.Cause, ErrIntegrity) && !errors.Is(failure.Cause, ErrValidation) &&
		!errors.Is(failure.Cause, ErrRepositoryViewSourceLimit) {
		return ErrValidation
	}
	return nil
}
