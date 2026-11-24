package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// BranchIDStore reads and stores the IDs Git keeps in each branch's own
// configuration for the repository holding cwd.
type BranchIDStore interface {
	LocalBranches(ctx context.Context, cwd string) ([]domain.LocalBranch, error)
	StoreBranchID(ctx context.Context, cwd string, assignment domain.BranchIDAssignment) error
	// WithBranchIDLock runs use while no other process assigns branch IDs in
	// the same repository.
	WithBranchIDLock(ctx context.Context, cwd string, use func() error) error
}

// BranchLedgerRemote registers with the server the IDs this CLI keeps for
// branches Git's records show on origin.
type BranchLedgerRemote interface {
	RegisterBranchIDs(ctx context.Context, repoID string, branches []domain.BranchRegistration) ([]domain.BranchRegistrationResult, error)
}

// WorktreeListRemote tells the server which worktrees the repository has on
// this computer, by name.
type WorktreeListRemote interface {
	ReportWorktrees(ctx context.Context, repoID, machine string, worktrees []domain.WorktreeRegistration) error
}

// CollectorReportRemote takes what the collector of this computer says about
// itself.
type CollectorReportRemote interface {
	ReportCollector(ctx context.Context, repoID string, report domain.CollectorReport) error
}

// CollectorStateStore keeps what the automatic collector last sent for the
// repository holding cwd. It is a memory, never a source of facts: a lost
// state only makes the next run send more.
type CollectorStateStore interface {
	LoadCollectorState(ctx context.Context, cwd string) (domain.CollectorState, error)
	SaveCollectorState(ctx context.Context, cwd string, state domain.CollectorState) error
}
