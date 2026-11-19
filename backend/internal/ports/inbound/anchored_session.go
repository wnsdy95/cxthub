package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSessions stores and reads the session reports CLIs upload. Until the
// rule that publishes a session when its commit is pushed exists, a report is
// readable only by the account that uploaded it.
type AnchoredSessions interface {
	PutAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error)
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error)
	AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error)
	// AnchoredSessionCalls lists the recorded agent calls that name one of the
	// caller's sessions as sender or receiver.
	AnchoredSessionCalls(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredAgentCall, error)
}
