package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// InitializeSelection handles a connected clone's first explicit pull. Only an
// exact code binding for the current branch tip can fill an empty cursor. This
// is separate from Preview, which must remain read-only.
func (s *SelectedPullService) InitializeSelection(ctx context.Context, in SelectedPullInput) error {
	repo, err := s.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if in.RepoID != "" && in.RepoID != repo.ID {
		return domain.ErrHashMismatch
	}
	state, err := s.store.ReadCheckoutState(ctx, repo.ID)
	if err != nil {
		return err
	}
	if p := state.Position; p != nil {
		if p.Snapshot != "" {
			return nil // An existing selection belongs to its worktree.
		}
		if p.Rewound || p.Orphan || p.SharedTarget != "" || p.MemoryHash != "" || p.MemorySource != "" || p.Selection != nil {
			return domain.ErrSelectionChanged
		}
	}
	store, ok := s.store.(outbound.InitialPullStore)
	if !ok {
		return fmt.Errorf("%w: initial pull selection is unavailable", domain.ErrSelectionChanged)
	}
	branch, err := s.git.CurrentBranch(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if domain.ValidateBranchName(branch) != nil {
		return domain.ErrCodePositionMismatch
	}
	code, err := s.code.CurrentCommit(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if !domain.ValidGitOID(code) {
		return domain.ErrCodePositionMismatch
	}
	binding, err := store.ResolveLocalBranch(ctx, repo.ID, branch)
	if err != nil {
		return err
	}
	if binding.Inactive {
		return domain.ErrBranchArchived
	}
	ref, err := store.GetRef(ctx, repo.ID, domain.RefBranch, binding.Branch)
	if err != nil {
		return err
	}
	// Reauthorize the complete projection before trusting any binding. Local
	// branch labels or cached history alone cannot establish server authority.
	view, _, err := s.readProjection(ctx, repo.ID, domain.ContextSelection{Branch: ref.Name, Position: string(ref.Target), CodeCommit: code, Scope: "current"}, &pullReadAnchor{})
	if err != nil {
		return err
	}
	bound := false
	for _, e := range view.History {
		if e.RepoID != repo.ID || e.Target != ref.Target || e.GitAfter != code || (ref.BranchID == "" && e.Branch != ref.Name) || (ref.BranchID != "" && e.BranchID != ref.BranchID) {
			continue
		}
		if e.Kind == "pr-merge" && !e.PRCompleted {
			continue
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return domain.ErrHashMismatch
		}
		bound = true
		break
	}
	if !bound {
		return fmt.Errorf("%w: the fetched branch tip has no exact binding to the current Git commit; select a verified code position first", domain.ErrCodePositionMismatch)
	}
	snapshot, err := store.GetSnapshot(ctx, ref.Target)
	if err != nil {
		return err
	}
	if snapshot.RepoID != repo.ID {
		return domain.ErrHashMismatch
	}
	actualBranch, err := s.git.CurrentBranch(ctx, in.Cwd)
	if err != nil {
		return err
	}
	actualCode, err := s.code.CurrentCommit(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if actualBranch != branch || actualCode != code {
		return domain.ErrCodePositionMismatch
	}
	actualRepo, err := s.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if actualRepo.ID != repo.ID {
		return domain.ErrSelectionChanged
	}
	return store.CommitCheckout(ctx, outbound.CheckoutTransition{
		ExpectedGitCommit: code, ExpectedGitBranch: branch,
		RepoID: repo.ID, Expected: state, ExpectedMemoryHash: snapshot.MemoryHash,
		Head: domain.Ref{RepoID: repo.ID, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: ref.Name}, Branch: &ref,
	})
}
