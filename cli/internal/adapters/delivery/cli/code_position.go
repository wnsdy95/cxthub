package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Context follows the checked-out code. Resolve recorded code associations
// along its first-parent ancestry; wall-clock proximity is never evidence.
func selectCodePosition(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil || operationInProgress(cwd) {
		return nil
	}
	branch := gitOut(cwd, "symbolic-ref", "--short", "HEAD")
	oid := gitOut(cwd, "rev-parse", "--verify", "HEAD")
	old, err := c.History.CurrentPosition(ctx)
	if err != nil && err != domain.ErrNotFound {
		return err
	}
	if err == nil && old.GitBranch() == branch && old.GitCommit == oid {
		return nil
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	localBranch := branch
	binding, err := c.History.ResolveLocalBranch(ctx, repo.ID, branch)
	if err != nil {
		return err
	}
	branch = binding.Branch
	if oid == "" {
		if old.Orphan && old.Branch == branch {
			return nil
		}
		all, err := c.List.List(ctx, inbound.ListInput{RepoID: repo.ID})
		if err != nil {
			return err
		}
		if len(all.Snapshots) > 0 {
			return fmt.Errorf("unborn Git branch has no verified context birth; replay its branch operation")
		}
		return c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo.ID, Branch: branch, Orphan: true})
	}
	all, err := c.List.List(ctx, inbound.ListInput{RepoID: repo.ID})
	if err != nil {
		return err
	}
	history, err := c.History.ListHistory(ctx, repo.ID)
	if err != nil {
		return err
	}
	selected := contextSelectionAtCode(cwd, oid, branch, all.Snapshots, history)
	id := selected.Snapshot
	if id == "" && len(all.Snapshots) > 0 {
		return fmt.Errorf("no recorded context on the ancestry of Git %s; current context preserved", oid)
	}
	selected.RepoID, selected.Branch, selected.GitCommit = repo.ID, branch, oid
	if localBranch != branch {
		selected.LocalBranch = localBranch
	}
	return c.History.SelectPosition(ctx, selected)
}

func contextAtCode(cwd, oid, branch string, snaps []domain.Snapshot, history []domain.HistoryEvent) domain.ContentHash {
	return contextSelectionAtCode(cwd, oid, branch, snaps, history).Snapshot
}

func contextSelectionAtCode(cwd, oid, branch string, snaps []domain.Snapshot, history []domain.HistoryEvent) domain.WorkingPosition {
	rewrites := loadRewrites(cwd)
	identity := ""
	if bindings, err := domain.ProjectContextBranches(history); err == nil {
		identity = bindings.Active[branch].ID
	}
	ancestry := strings.Fields(gitOut(cwd, "rev-list", "--first-parent", oid))
	for _, code := range ancestry {
		var best domain.ContentHash
		var selected domain.WorkingPosition
		var observed time.Time
		// Prefer a recorded target on this logical branch; include history roots
		// because the associated snapshot may no longer be on the shared tip path.
		for i := len(history) - 1; i >= 0; i-- {
			e := history[i]
			if e.Kind == "publish" {
				continue
			} // Delivery completion never changes memory selection.
			matches := e.Branch == branch
			if identity != "" {
				matches = e.BranchID == identity
			}
			if matches && e.GitAfter == code && e.Target != "" && (best == "" || e.CreatedAt.After(observed)) {
				best = e.Target
				selected = domain.WorkingPosition{Snapshot: e.Target, MemoryHash: e.MemoryHash, MemorySource: e.MemorySource, MemoryPinned: e.MemoryPinned}
				observed = e.CreatedAt
			}
		}
		if best != "" {
			return selected
		}
		for _, sameBranch := range []bool{true, false} {
			for _, snap := range snaps {
				if (snap.Branch == branch) != sameBranch {
					continue
				}
				match := gitLinkRe.FindStringSubmatch(snap.Message)
				if match == nil {
					continue
				}
				linked := resolveRewritten(rewrites, match[1])
				if strings.HasPrefix(code, linked) && (best == "" || snap.CreatedAt.After(observed)) {
					best = snap.ID
					selected = domain.WorkingPosition{Snapshot: snap.ID}
					observed = snap.CreatedAt
				}
			}
			if best != "" {
				return selected
			}
		}
	}
	return domain.WorkingPosition{}
}
