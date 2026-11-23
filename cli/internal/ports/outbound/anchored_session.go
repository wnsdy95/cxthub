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

// AnchoredRemovalRemote reads and changes which of the caller's sessions are
// removed on the server.
type AnchoredRemovalRemote interface {
	// ListRemovedAnchoredSessions lists the caller's own removed sessions.
	// A server that has no such list answers none.
	ListRemovedAnchoredSessions(ctx context.Context, repoID string) ([]domain.AnchoredSessionRemoval, error)
	// RemoveAnchoredSession removes a session and the sessions it spawned
	// and answers what was removed, the session asked for first.
	RemoveAnchoredSession(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredSessionRemoval, error)
	// AllowAnchoredSession takes back the removal of one session.
	AllowAnchoredSession(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string) error
}
