package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// CaptureDocumentPolicy selects the identity for a NEW local capture after
// repository resolution. Implementations must be local-only: capture can hold
// the config/capture gate. This preference never authorizes publication.
type CaptureDocumentPolicy interface {
	CaptureDocumentIdentity(context.Context, domain.Repo) (domain.DocumentIdentity, error)
}
