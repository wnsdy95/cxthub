package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// WorkingPositionQueryReader observes the persisted worktree cursor without
// lock creation, mutation recovery, branch reconciliation or registration.
type WorkingPositionQueryReader interface {
	ReadWorkingPosition(context.Context, string) (domain.WorkingPosition, error)
}
