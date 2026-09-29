package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// CheckoutState is the exact selection observed before provider preparation.
// Position is nil for a replica without a worktree cursor. Reads never replay.
type CheckoutState struct {
	IndexRevision domain.ContentHash      `json:"index_revision,omitempty"`
	Head          domain.Ref              `json:"head"`
	HeadMissing   bool                    `json:"head_missing"`
	Position      *domain.WorkingPosition `json:"position,omitempty"`
}

// CheckoutTransition publishes a prepared selection. Branch is non-nil for a
// named selection; CreateBranch requires that it is still absent at commit.
type CheckoutTransition struct {
	// Fence the Git identity captured when the worktree store was constructed.
	// A Git move before preparation requires a fresh command, never a cursor
	// pairing newly verified context with stale process-start code metadata.
	ExpectedGitCommit string `json:"expected_git_commit,omitempty"`
	ExpectedGitBranch string `json:"expected_git_branch,omitempty"`
	// ExpectedMemoryHash is the target attachment observed before preparation.
	// Empty means no attachment; it never disables the comparison.
	ExpectedMemoryHash domain.ContentHash `json:"expected_memory_hash"`
	// MemoryPin preserves historical prepared memory independently of the
	// snapshot's current attachment. Nil selects ExpectedMemoryHash.
	MemoryPin    *domain.AgentMemoryPin `json:"memory_pin,omitempty"`
	RestoreEvent *domain.HistoryEvent   `json:"restore_event,omitempty"`
	RepoID       string                 `json:"repo_id"`
	Expected     CheckoutState          `json:"expected"`
	Head         domain.Ref             `json:"head"`
	Branch       *domain.Ref            `json:"branch,omitempty"`
	CreateBranch bool                   `json:"create_branch,omitempty"`
}

// CheckoutPins retains snapshots/documents owned by an unfinished checkout.
// Reads never recover the journal; corrupt/unknown journals must fail closed.
// Collection calls this while holding the exclusive object retention lease.
type CheckoutPins interface {
	HasCheckoutPin(context.Context, domain.ContentHash) (bool, error)
}

type CheckoutTransactionStore interface {
	ReadCheckoutState(context.Context, string) (CheckoutState, error)
	CommitCheckout(context.Context, CheckoutTransition) error
}
