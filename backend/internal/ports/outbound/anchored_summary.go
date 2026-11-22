package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSummaryStore keeps the summary of a session at each commit a
// segment of it ends at. Who may read one is decided by the application.
type AnchoredSummaryStore interface {
	// PutAnchoredSummary stores the summary of one session at one commit,
	// replacing the one stored before.
	PutAnchoredSummary(ctx context.Context, summary domain.AnchoredSummary) error
	// GetAnchoredSummary returns ErrNotFound when the session has no summary
	// at the commit.
	GetAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string) (domain.AnchoredSummary, error)
	// ListCommitAnchoredSummaries lists the summaries stored at the commits,
	// by commit, tool, then session.
	ListCommitAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error)
}
