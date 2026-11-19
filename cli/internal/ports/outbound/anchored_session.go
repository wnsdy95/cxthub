package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// AnchoredSessionRemote stores one session's report on the server. The report
// replaces the session's earlier report as a whole; the server answers
// whether anything changed.
type AnchoredSessionRemote interface {
	PutAnchoredSession(ctx context.Context, repoID string, upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error)
}
