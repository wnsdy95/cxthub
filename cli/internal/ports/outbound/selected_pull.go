package outbound

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// SelectedPullPlan pins an authorized server projection to one exact worktree
// selection. It is not a replacement for any immutable snapshot or digest.
type SelectedPullPlan struct {
	Version   int                          `json:"version"`
	ID        domain.ContentHash           `json:"id"`
	RepoID    string                       `json:"repo_id"`
	Remote    string                       `json:"remote"`
	Expected  CheckoutState                `json:"expected"`
	Previous  domain.ContentHash           `json:"previous,omitempty"`
	Selection domain.ContextSelection      `json:"selection"`
	Context   domain.ContextQueryView      `json:"context"`
	Memory    []domain.EffectiveMemoryPage `json:"memory"`
}

// SelectedPullReceipt is a local application receipt, not a server write
// acknowledgement. Context and memory bodies are self-contained projections;
// referenced raw documents may need an authorized server fetch when opened.
type SelectedPullReceipt struct {
	Plan      SelectedPullPlan `json:"plan"`
	AppliedAt time.Time        `json:"applied_at"`
}

func SelectedPullPlanID(plan SelectedPullPlan) domain.ContentHash {
	plan.ID = ""
	raw, _ := json.Marshal(plan)
	return domain.HashContent(raw)
}

// SelectedPullStore publishes the receipt atomically only if the worktree,
// index and previous applied receipt still match. Reads never recover writes.
type SelectedPullStore interface {
	ReadCheckoutState(context.Context, string) (CheckoutState, error)
	ReadAppliedPull(context.Context, string, string) (SelectedPullReceipt, error)
	ApplySelectedPull(context.Context, SelectedPullPlan) (SelectedPullReceipt, error)
}

// InitialPullStore may initialize an empty worktree after verified objects have
// arrived. Existing selections are never replaced by this operation.
type InitialPullStore interface {
	CheckoutTransactionStore
	LocalBranchReader
	GetRef(context.Context, string, domain.RefKind, string) (domain.Ref, error)
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
}
