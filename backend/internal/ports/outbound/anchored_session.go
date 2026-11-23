package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSessionStore keeps the reports CLIs upload for coding-agent
// sessions, and an index from each commit to the sessions cut at it. Putting a
// report replaces the session's earlier report and its index entries together,
// and leaves the session's archive mark as it is.
type AnchoredSessionStore interface {
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSession, error)
	// ListRepositoryAnchoredSessions lists the reports of every uploader, by tool then identifier.
	ListRepositoryAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSession, error)
	PutAnchoredSession(ctx context.Context, session domain.AnchoredSession) error
	// SetAnchoredSessionArchive archives the session at that time by that
	// account, or restores it when the time is nil. A session that is not
	// stored is ErrNotFound.
	SetAnchoredSessionArchive(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, at *time.Time, by string) error
	ListAnchoredCommitLinks(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitLink, error)
	// DeleteAnchoredSession removes a session's report together with its
	// commit index entries, its summaries and its open ranges. A session
	// that is not stored is ErrNotFound.
	DeleteAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error
	// AnchoredContentInUse reports whether the document is still needed in
	// the repository: a stored report names it as its content.
	AnchoredContentInUse(ctx context.Context, repo, doc domain.ContentHash) (bool, error)
}

// AnchoredRemovalStore remembers which sessions were removed from the server.
// A session has at most one removal.
type AnchoredRemovalStore interface {
	// PutAnchoredSessionRemoval records a removal. One that already stands
	// for the session is kept as it is.
	PutAnchoredSessionRemoval(ctx context.Context, removal domain.AnchoredSessionRemoval) error
	// GetAnchoredSessionRemoval answers ErrNotFound for a session that was
	// not removed.
	GetAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionRemoval, error)
	// DeleteAnchoredSessionRemoval lets the session be uploaded again. A
	// session that was not removed is ErrNotFound.
	DeleteAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error
	// ListAnchoredSessionRemovals lists the removals of sessions that
	// belonged to uploader, by tool then identifier.
	ListAnchoredSessionRemovals(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSessionRemoval, error)
}
