package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CollectorStore keeps what the collectors of a repository said about
// themselves, and the pushes they said their clones made.
type CollectorStore interface {
	// PutCollectorReport stores a collector in place of what was stored for
	// the same account and machine, and its claims: a push nobody claimed
	// yet is claimed, and one that is claimed already is merged by
	// domain.MergeClaimedPush.
	PutCollectorReport(ctx context.Context, repo domain.ContentHash, collector domain.ReportedCollector, claims []domain.ClaimedPush) error
	// ListReportedCollectors lists the collectors by account, then machine.
	ListReportedCollectors(ctx context.Context, repo domain.ContentHash) ([]domain.ReportedCollector, error)
	// ListClaimedPushes lists at most limit claimed pushes, the one pushed
	// last first.
	ListClaimedPushes(ctx context.Context, repo domain.ContentHash, limit int) ([]domain.ClaimedPush, error)
}
