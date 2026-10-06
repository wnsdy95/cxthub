package outbound

import "context"

// CapturePositionStore admits a connected repository before capture begins.
// Call before WithCaptureTrackingGate; a mismatched nested call must fail.
// This only normalizes a proven empty init cursor, never adopts branch context.
type CapturePositionStore interface {
	EnsureCapturePosition(context.Context, string) error
}
