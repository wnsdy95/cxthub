package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSessionStore keeps the reports CLIs upload for coding-agent
// sessions, and an index from each commit to the sessions cut at it. Putting a
// report replaces the session's earlier report and its index entries together.
type AnchoredSessionStore interface {
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSession, error)
	// ListRepositoryAnchoredSessions lists the reports of every uploader, by tool then identifier.
	ListRepositoryAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSession, error)
	PutAnchoredSession(ctx context.Context, session domain.AnchoredSession) error
	ListAnchoredCommitLinks(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitLink, error)
}
