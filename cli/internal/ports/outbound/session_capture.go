package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"time"
)

// SessionCapture owns native transcript access, masking and disposable projection
// checkpoints. The application owns snapshot/ref publication after Project has
// durably stored the normalized document. No branch may move on projection error.
type SessionCapture interface {
	Eligible(root, path string) bool
	Project(ctx context.Context, root, path string, source CaptureSource, codec ProviderCodec, allowPartial bool) (domain.Envelope, domain.ContentHash, int64, *time.Time, error)
	Settings(root, kind string) (domain.SettingsBundle, bool)
	RecordAffinity(root string, provider domain.ProviderKind, sessionID string)
}
