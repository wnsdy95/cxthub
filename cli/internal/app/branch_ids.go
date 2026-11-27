package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// EnsureBranchIDs gives an ID to every branch of the repository holding cwd
// that has none of its own, and returns the branches as Git holds them
// afterwards. It writes only the branches' own configuration.
func EnsureBranchIDs(ctx context.Context, store outbound.BranchIDStore, cwd string, newID func() (string, error)) ([]domain.LocalBranch, int, error) {
	if store == nil || newID == nil {
		return nil, 0, fmt.Errorf("branch IDs unavailable")
	}
	var branches []domain.LocalBranch
	assigned := 0
	err := store.WithBranchIDLock(ctx, cwd, func() error {
		read, err := store.LocalBranches(ctx, cwd)
		if err != nil {
			return err
		}
		var failure error
		plan := domain.PlanBranchIDs(read, func() string {
			id, err := newID()
			if err != nil && failure == nil {
				failure = err
			}
			return id
		})
		if failure != nil {
			return failure
		}
		if len(plan) == 0 {
			branches = read
			return nil
		}
		for _, assignment := range plan {
			if err := store.StoreBranchID(ctx, cwd, assignment); err != nil {
				return err
			}
			assigned++
		}
		// What Git holds now is the answer, not what this call meant to write.
		branches, err = store.LocalBranches(ctx, cwd)
		return err
	})
	if err != nil {
		return nil, assigned, err
	}
	return branches, assigned, nil
}
