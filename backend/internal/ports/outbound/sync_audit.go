package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// GitSyncReader performs bounded, read-only queries to the repository's trusted
// origin. Provider adapters never decide context lineage or repair refs.
type GitSyncReader interface {
	ListAuditPRs(context.Context, string, int) (GitAuditPRPage, error)
	ReadAuditCommit(context.Context, string, string) (string, error)
	ReadAuditPR(context.Context, string, int) (domain.PullRequestMerge, bool, error)
}

// Anchor covers every row, including unmerged closed PRs that affect pagination.
type GitAuditPRPage struct {
	PRs    []domain.PullRequestMerge
	More   bool
	Count  int
	Anchor string
}
