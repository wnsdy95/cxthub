package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// CommitCapture freezes private input separately from normalization. Replay
// retains the original code/memory attribution, never a newly discovered source.
type CommitCapture interface {
	FreezeInput(context.Context, string, domain.ProviderKind, string) (domain.FrozenCaptureInput, error)
	FreezeSettings(context.Context, string) (map[string]domain.ContentHash, error)
	RecordFrozenContinuation(context.Context, string, domain.CaptureAttempt) error
	PrepareFrozenMemory(context.Context, string, domain.CaptureAttempt, int) (*domain.FrozenCaptureMemory, error)
	FinishFrozenMemory(context.Context, string, domain.CaptureAttempt) (domain.HistoryEvent, error)
	SaveFrozen(context.Context, string, domain.CaptureAttempt, int) (SaveOutput, error)
	ApplyFrozen(context.Context, string, domain.CaptureAttempt) error
}
