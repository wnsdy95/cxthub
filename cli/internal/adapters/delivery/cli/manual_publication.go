package cli

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// A manual selector names the canonical context ref, never a Git tracking
// alias or HEAD. Freeze its identity before replay and carry it into Push.
func prepareManualPublication(ctx context.Context, c *Container, cwd, name string) (domain.PublicationScope, error) {
	var scope domain.PublicationScope
	if c.Queries == nil || c.History == nil {
		return scope, fmt.Errorf("selected context publication requires local refs and history")
	}
	if err := domain.ValidateBranchName(name); err != nil {
		return scope, err
	}
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return scope, err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repo.ID)); err != nil {
		return scope, err
	}
	// Replay uses the current worktree only to locate the shared replica. It
	// must not silently skip all selected journals when that locator is absent.
	position, err := c.History.CurrentPosition(ctx)
	if err != nil {
		return scope, err
	}
	if position.RepoID != repo.ID || position.WorktreeID == "" {
		return scope, domain.ErrSelectionChanged
	}
	resolve := func() (domain.PublicationBranch, error) {
		refs, err := c.Queries.Refs(ctx, cwd)
		if err != nil {
			return domain.PublicationBranch{}, err
		}
		var ref domain.Ref
		found := false
		for _, r := range refs {
			if r.Kind != domain.RefBranch || r.Name != name {
				continue
			}
			if r.RepoID != repo.ID || domain.ValidateRef(r) != nil || (found && r != ref) {
				return domain.PublicationBranch{}, domain.ErrHashMismatch
			}
			ref, found = r, true
		}
		if !found {
			return domain.PublicationBranch{}, domain.ErrNotFound
		}
		if ref.BranchID == "" {
			events, err := c.History.ListHistory(ctx, repo.ID)
			if err != nil {
				return domain.PublicationBranch{}, err
			}
			projection, err := domain.ProjectContextBranches(events)
			if err != nil {
				return domain.PublicationBranch{}, err
			}
			ref.BranchID = projection.Identity(repo.ID, name)
		}
		return domain.PublicationBranch{Branch: name, BranchID: ref.BranchID}, nil
	}
	selected, err := resolve()
	if err != nil {
		return scope, err
	}
	if err := replayRewriteHistoryForBranches(ctx, c, cwd, map[string]bool{selected.BranchID: true}); err != nil {
		return scope, err
	}
	current, err := resolve()
	if err != nil {
		return scope, err
	}
	if current != selected {
		return scope, domain.ErrSelectionChanged
	}
	scope.Branches = []domain.PublicationBranch{selected}
	return scope, nil
}
