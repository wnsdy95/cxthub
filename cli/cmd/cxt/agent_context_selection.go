package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/app"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// selectAgentContext resolves only an implicit current selection. An explicit
// hash stays detached even if HEAD happens to point at the same snapshot.
func (r runtimeAgentPreparer) selectAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (inbound.PrepareAgentContextInput, error) {
	if in.LatestMain || !in.ArtifactOnly {
		if r.store == nil {
			return in, domain.ErrAgentContextUnavailable
		}
		code, ok := r.git.(outbound.CodePosition)
		if !ok {
			return in, domain.ErrAgentContextUnavailable
		}
		commit, err := code.CurrentCommit(ctx, in.Cwd)
		if err != nil {
			return in, err
		}
		if !domain.ValidGitOID(commit) {
			return in, domain.ErrCodePositionMismatch
		}
		branch, err := r.git.CurrentBranch(ctx, in.Cwd)
		if err != nil {
			return in, err
		}
		_, state, _, err := r.agentWorktreeState(ctx, in.Cwd, in.RepoID)
		if err != nil {
			return in, err
		}
		if in.WorktreeStateHash != "" && in.WorktreeStateHash != state {
			return in, domain.ErrSelectionChanged
		}
		observedBranch, err := r.git.CurrentBranch(ctx, in.Cwd)
		if err != nil {
			return in, err
		}
		if observedBranch != branch {
			return in, domain.ErrSelectionChanged
		}
		in.LatestMain, in.Branch, in.SnapshotID, in.MemoryPin = true, "main", "", nil
		in.WorktreeStateHash = state
		in.WorkingPosition = &domain.AgentWorkingPosition{Branch: branch, CodeCommit: commit}
		return in, nil
	}
	if r.store == nil {
		return in, nil
	}
	if in.Branch != "" {
		binding, err := r.store.ResolveLocalBranch(ctx, in.RepoID, in.Branch)
		if err != nil {
			return in, err
		}
		if binding.Inactive {
			return in, domain.ErrBranchArchived
		}
		in.Branch = binding.Branch
	}
	p, state, present, err := r.agentWorktreeState(ctx, in.Cwd, in.RepoID)
	if err != nil {
		return in, err
	}
	current := in.SnapshotID == "" && in.Branch == ""
	if current && present {
		if p.Snapshot == "" {
			return in, fmt.Errorf("%w: selected worktree has no context snapshot; refusing to select a branch tip", domain.ErrAgentContextUnavailable)
		}
		in.Branch, in.SnapshotID = p.Branch, p.Snapshot
	}
	// Historical selections keep an exact memory revision even after receiving a
	// branch name. Ordinary positions also record MemoryPinned for local recovery,
	// but their input uses the server's code-scoped integrated branch projection.
	pinned := in.MemoryPin == nil && present && p.Rewound && p.Snapshot == in.SnapshotID && (in.Branch == "" || in.Branch == p.Branch)
	if pinned {
		in.MemoryPin = &domain.AgentMemoryPin{}
		if p.MemoryHash != "" {
			owner := p.MemorySource
			if owner == "" {
				owner = p.Snapshot
			}
			in.MemoryPin.SnapshotID, in.MemoryPin.MemoryHash = owner, p.MemoryHash
		}
	}
	if current || pinned {
		if in.WorktreeStateHash != "" && in.WorktreeStateHash != state {
			return in, domain.ErrSelectionChanged
		}
		in.WorktreeStateHash = state
	}
	return in, nil
}

// Hash only this worktree's selection, not moving shared heads or pending tails.
// Include the actual Git branch so a same-SHA checkout also invalidates delivery.
func (r runtimeAgentPreparer) agentWorktreeState(ctx context.Context, cwd, repo string) (domain.WorkingPosition, domain.ContentHash, bool, error) {
	branch, err := r.git.CurrentBranch(ctx, cwd)
	if err != nil {
		return domain.WorkingPosition{}, "", false, err
	}
	p, err := r.store.ReadWorkingPosition(ctx, repo)
	present := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return p, "", false, err
	}
	if !present {
		p = domain.WorkingPosition{}
	}
	selected := p
	selected.SharedTarget = ""
	selected.Selection = nil
	raw, err := json.Marshal(struct {
		GitBranch string
		Present   bool
		Position  domain.WorkingPosition
	}{branch, present, selected})
	if err != nil {
		return p, "", present, err
	}
	return p, domain.HashContent(raw), present, nil
}

func (r runtimeAgentPreparer) validateAgentWorktree(ctx context.Context, cwd, repo string, expected domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == "" {
		return nil // explicit source independent of the current worktree cursor
	}
	if r.store == nil {
		return domain.ErrAgentContextUnavailable
	}
	_, actual, _, err := r.agentWorktreeState(ctx, cwd, repo)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%w: worktree selection or pinned memory changed during preparation", domain.ErrSelectionChanged)
	}
	return nil
}

func (r runtimeAgentPreparer) validateAgentDelivery(ctx context.Context, cwd string, selected domain.AgentContextSelection) error {
	code, ok := r.git.(outbound.CodePosition)
	if !ok || selected.WorktreeStateHash == "" {
		return domain.ErrAgentContextUnavailable
	}
	actual, err := code.CurrentCommit(ctx, cwd)
	if err != nil {
		return err
	}
	if !domain.ValidGitOID(actual) || actual != selected.DeliveryCodeCommit() {
		return domain.ErrCodePositionMismatch
	}
	if err := r.validateAgentWorktree(ctx, cwd, selected.RepositoryID, selected.WorktreeStateHash); err != nil {
		return err
	}
	if selected.SourcePolicy == domain.AgentSourceLatestMain {
		branch, err := r.git.CurrentBranch(ctx, cwd)
		if err != nil {
			return err
		}
		if selected.WorkingPosition == nil || branch != selected.WorkingPosition.Branch {
			return domain.ErrSelectionChanged
		}
		service := app.NewAgentContextService(r.history, r.remote, nil, nil, nil, nil)
		if err := service.ValidateLatestMain(ctx, cwd, selected); err != nil {
			return err
		}
		// Both Git and the cursor may change while the authorized reads run.
		actual, err = code.CurrentCommit(ctx, cwd)
		if err != nil {
			return err
		}
		if actual != selected.DeliveryCodeCommit() {
			return domain.ErrCodePositionMismatch
		}
		return r.validateAgentWorktree(ctx, cwd, selected.RepositoryID, selected.WorktreeStateHash)
	}
	return nil
}

func (r runtimeAgentPreparer) ValidateAgentContextDelivery(ctx context.Context, cwd string, selected domain.AgentContextSelection) error {
	return r.validateAgentDelivery(ctx, cwd, selected)
}
