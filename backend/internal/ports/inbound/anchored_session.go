package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSessions stores and reads the session reports CLIs upload. A report
// is readable in full by the account that uploaded it; another member reads
// what is open of it: the parts whose commit, or branch, was pushed.
type AnchoredSessions interface {
	PutAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error)
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error)
	AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error)
	// AnchoredSessionCalls lists the recorded agent calls that name one of the
	// caller's sessions as sender or receiver.
	AnchoredSessionCalls(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredAgentCall, error)
	// AnchoredSessionContentAccess says what the caller may read of a
	// session's conversation.
	AnchoredSessionContentAccess(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredContentAccess, error)
}
