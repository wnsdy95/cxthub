package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type BranchPullPlanner interface {
	BranchPullVersion() int
	PullBranchPlan(context.Context, domain.ContentHash, domain.BranchPullRequest) (domain.BranchPullPlan, error)
}
