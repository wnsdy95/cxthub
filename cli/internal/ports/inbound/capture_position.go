package inbound

import "context"

// ContextCaptureAdmission normalizes an empty provisional cursor before live
// preparation or a capture intent freezes its repository identity.
type ContextCaptureAdmission interface {
	EnsureCapturePosition(context.Context, string) error
}

// ContextCaptureGate excludes initial attachment/normalization while a caller
// rechecks and persists capture evidence. Use the callback context for local
// work only; do not run admission or network operations in the callback.
type ContextCaptureGate interface {
	WithCaptureTrackingGate(context.Context, func(context.Context) error) error
}
