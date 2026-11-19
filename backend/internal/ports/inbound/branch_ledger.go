package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// BranchLedger registers the IDs members' CLIs keep for branches that are on
// the remote, and reads the lifetimes of branch names.
type BranchLedger interface {
	RegisterBranchIDs(ctx context.Context, repo domain.ContentHash, branches []domain.BranchRegistration) ([]domain.BranchRegistrationResult, error)
	ListBranchLifetimes(ctx context.Context, repo domain.ContentHash, name string) ([]domain.BranchLifetime, error)
}
