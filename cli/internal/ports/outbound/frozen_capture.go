package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// FrozenSessionCapture is an optional durable native-input capture capability.
// Freeze pins exact native bytes in a private spool, never the normal CAS. The
// application must durably journal the returned descriptor before deferring work.
// ProjectFrozen verifies current spool bytes and masking policy on every call;
// no previous projection receipt authorizes reuse or snapshot publication.
type FrozenSessionCapture interface {
	Freeze(ctx context.Context, root, path string, provider domain.ProviderKind, identity domain.DocumentIdentity) (domain.FrozenCaptureInput, error)
	// FrozenSessionID inspects only a verified frozen header (1 MiB / 512 lines).
	// Empty means no identity was found; full CIR verification still follows.
	FrozenSessionID(ctx context.Context, root string, input domain.FrozenCaptureInput) (string, error)
	// ScrubFrozenMemory copies private receipt memory under the pinned policy.
	// Nil stays absent; this must never rediscover native provider memory.
	ScrubFrozenMemory(ctx context.Context, root string, input domain.FrozenCaptureInput) (*domain.NativeMemory, error)
	ProjectFrozen(ctx context.Context, root string, input domain.FrozenCaptureInput, source CaptureSource, codec ProviderCodec) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error)
}
