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

// SegmentCareRemote is what the commands that find and put right what is
// wrong ask of the server, beyond removals.
type SegmentCareRemote interface {
	AnchoredRemovalRemote
	// ListAnchoredSessions lists the sessions the caller may read; the
	// caller's own are those that are not shared.
	ListAnchoredSessions(ctx context.Context, repoID string) ([]domain.AnchoredSessionListed, error)
	// ListAnchoredCommitSummaries lists the summaries at one commit that the
	// caller may read.
	ListAnchoredCommitSummaries(ctx context.Context, repoID, commit string) ([]domain.AnchoredSummary, error)
	// ReadRemoteGitSyncedAt says when the server last finished reading the
	// repository from GitHub; empty when it never did.
	ReadRemoteGitSyncedAt(ctx context.Context, repoID string) (string, error)
	// RequestRemoteGitRefresh asks the server to read GitHub again soon.
	RequestRemoteGitRefresh(ctx context.Context, repoID string) error
	// SetAnchoredSessionArchived archives a session or restores it.
	SetAnchoredSessionArchived(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string, archived bool) error
}
