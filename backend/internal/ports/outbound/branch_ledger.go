package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// BranchLedgerStore keeps the branch lifetimes of a repository. Every ID, a
// lifetime's own or one joined to it, belongs to one lifetime, and at most one
// lifetime is alive under a name.
type BranchLedgerStore interface {
	// AliveBranchLifetime returns the lifetime alive under name, or ErrNotFound.
	AliveBranchLifetime(ctx context.Context, repo domain.ContentHash, name string) (domain.BranchLifetime, error)
	// BranchLifetimeHolding returns the lifetime that holds id, or ErrNotFound.
	BranchLifetimeHolding(ctx context.Context, repo domain.ContentHash, id string) (domain.BranchLifetime, error)
	// ListBranchLifetimes lists lifetimes oldest first; of one name when name
	// is not empty.
	ListBranchLifetimes(ctx context.Context, repo domain.ContentHash, name string) ([]domain.BranchLifetime, error)
	// PutBranchLifetime stores a lifetime, replacing the one with the same ID,
	// together with the IDs it holds.
	PutBranchLifetime(ctx context.Context, lifetime domain.BranchLifetime) error
}
