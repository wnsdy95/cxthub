package domain

import (
	"errors"
	"fmt"
)

// ErrIndexChanged is an exact staging revision mismatch, not a remote ref
// conflict. Keep errors.Is compatibility with existing conflict handling.
var ErrIndexChanged = fmt.Errorf("staging index changed: %w", ErrSyncConflict)

// ErrCodePositionMismatch identifies an observed Git commit mismatch. Other
// context, branch, or server revision changes remain ErrSelectionChanged.
var ErrCodePositionMismatch = fmt.Errorf("code position mismatch: %w", ErrSelectionChanged)

// ErrDeliveryFailed identifies failed prepared input encoding/materialization,
// launch validation/start, or delivery receipt persistence. It never proves
// rollback, input acceptance, or failure of a provider's later task execution.
var ErrDeliveryFailed = errors.New("provider input delivery failed")
