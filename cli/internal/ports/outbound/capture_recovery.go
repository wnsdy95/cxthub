package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type CaptureRecoveryJournal interface {
	ListCaptureAttempts(context.Context, string) ([]domain.CaptureAttempt, error)
	ReadCaptureResolution(context.Context, domain.CaptureAttempt) (*domain.CaptureResolution, error)
	// Compare all evidence fingerprints under the shared journal lock. The
	// original journals remain unchanged; the resolution is an immutable sidecar.
	WriteCaptureResolution(context.Context, domain.CaptureResolution, []domain.CaptureAttempt) error
}

type CaptureRecoveryEvidence interface {
	ListHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error)
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
}

// Retry diagnostics are separate from immutable input/proof fingerprints.
type CaptureRetryReader interface {
	ReadCaptureRetry(context.Context, domain.CaptureAttempt) (*domain.CaptureRetryState, error)
}
