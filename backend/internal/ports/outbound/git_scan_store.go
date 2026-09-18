package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

// GitScanStore keeps immutable provider deltas and restartable discovery work.
// Finish atomically fences the worker and publishes all dependent work in PG.
type GitScanStore interface {
	EnqueueGitScan(context.Context, domain.GitScanJob) error
	GetGitScan(context.Context, domain.ContentHash, string) (domain.GitScanJob, error)
	ListGitScans(context.Context, domain.ContentHash, string, int) ([]domain.GitScanJob, error)
	ClaimGitScan(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.GitScanJob, error)
	FinishGitScan(context.Context, domain.GitScanFinish) error
	RetryGitScan(context.Context, domain.ContentHash, string, time.Time) error
	FindGitInverses(context.Context, domain.ContentHash, string, string, string, int) ([]domain.GitInverseCandidate, error)
	GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error)
	RecordGitRefObservation(context.Context, domain.GitRefObservation, time.Time) error
}

// GitCommitReader returns every explicit parent comparison, with complete
// trees. No arbitrary merge mainline is silently selected.
type GitCommitReader interface {
	ReadCommitDeltas(context.Context, string, string) ([]domain.GitCommitDelta, error)
	ListGitHeads(context.Context, string, int) ([]GitHead, bool, error)
}
type GitHead struct {
	Ref    string
	Commit string
}

// A page and all its observed heads are committed before its cursor advances.
// Periodic first-page reconciliation recovers missing webhook deliveries; it
// does not infer deletion from a changing provider list or move any context ref.
type GitHeadScanStore interface {
	GetGitHeadScan(context.Context, domain.ContentHash, string) (domain.GitHeadScan, error)
	FailGitHeadScan(context.Context, domain.GitHeadScan, time.Time) error
	ClaimGitHeadScan(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitHeadScan, error)
	FinishGitHeadScan(context.Context, domain.GitHeadScan, []domain.GitRefObservation, bool, time.Time) error
}
