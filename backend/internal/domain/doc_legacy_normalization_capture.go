package domain

import (
	"fmt"
	"math"
	"sync/atomic"
)

var ErrLegacyNormalizationBusy = fmt.Errorf("%w: legacy normalization is busy", ErrConflict)

type legacyNormalizationMutation struct {
	stepping atomic.Bool
	activity atomic.Int64
	capture  atomic.Pointer[legacyNormalizationCapture]
	closed   atomic.Bool
}

type legacyNormalizationCapture struct {
	scope      *legacyNormalizationMutation
	references atomic.Int64
}

func (scope *legacyNormalizationMutation) enter() error {
	if scope == nil {
		return ErrIntegrity
	}
	if scope.closed.Load() {
		return ErrConflict
	}
	activity := scope.activity.Load()
	if activity < 0 {
		return ErrLegacyNormalizationBusy
	}
	if activity == math.MaxInt64 {
		return ErrIntegrity
	}
	if !scope.activity.CompareAndSwap(activity, activity+1) {
		return ErrLegacyNormalizationBusy
	}
	if scope.closed.Load() {
		scope.leave()
		return ErrConflict
	}
	return nil
}

func (scope *legacyNormalizationMutation) leave() {
	scope.activity.Add(-1)
}

func (scope *legacyNormalizationMutation) beginCapture() (*legacyNormalizationCapture, error) {
	if scope == nil {
		return nil, ErrIntegrity
	}
	if scope.closed.Load() {
		return nil, ErrConflict
	}
	if scope.activity.CompareAndSwap(0, -1) {
		capture := &legacyNormalizationCapture{scope: scope}
		capture.references.Store(1)
		scope.capture.Store(capture)
		if scope.closed.Load() {
			capture.release()
			return nil, ErrConflict
		}
		return capture, nil
	}
	capture := scope.capture.Load()
	if capture == nil || capture.scope != scope || scope.activity.Load() != -1 {
		return nil, ErrLegacyNormalizationBusy
	}
	references := capture.references.Load()
	if references <= 0 || references == math.MaxInt64 || !capture.references.CompareAndSwap(references, references+1) {
		return nil, ErrLegacyNormalizationBusy
	}
	if scope.closed.Load() {
		capture.release()
		return nil, ErrConflict
	}
	return capture, nil
}

func (capture *legacyNormalizationCapture) valid(scope *legacyNormalizationMutation) bool {
	return capture != nil && scope != nil && !scope.closed.Load() && capture.scope == scope &&
		scope.activity.Load() == -1 && scope.capture.Load() == capture && capture.references.Load() > 0
}

func (scope *legacyNormalizationMutation) stop() {
	if scope != nil {
		scope.closed.Store(true)
	}
}

func (capture *legacyNormalizationCapture) release() {
	if capture.references.Add(-1) == 0 {
		capture.scope.capture.CompareAndSwap(capture, nil)
		capture.scope.activity.CompareAndSwap(-1, 0)
	}
}
