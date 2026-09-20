package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// A completion's Source is the immutable context bound to the PR head. Target
// can be a newer base tip when the source was already reachable there.
func completedPRSelection(code, branch, identity string, history []domain.HistoryEvent) (domain.WorkingPosition, error) {
	var selected domain.WorkingPosition
	for _, e := range history {
		matches := e.Branch == branch
		if identity != "" {
			matches = e.BranchID == identity
		}
		if !matches || e.Kind != "pr-merge" || !e.PRCompleted || e.PR == nil || e.PR.MergeSHA != code || e.Source == "" {
			continue
		}
		if selected.Snapshot != "" && selected.Snapshot != e.Source {
			return domain.WorkingPosition{}, fmt.Errorf("conflicting completed PR sources for Git %s", code)
		}
		selected = domain.WorkingPosition{Snapshot: e.Source, GitCommit: code}
	}
	return selected, nil
}

// Resolve only the ordinary observations belonging to the frozen PR source.
// The application service follows memory's causal chain, not wall-clock order
// or the snapshot's mutable current attachment.
func resolveCompletedPRMemory(ctx context.Context, c *Container, branch, identity string, selected domain.WorkingPosition, history []domain.HistoryEvent) (domain.WorkingPosition, error) {
	if selected.GitCommit == "" || selected.MemoryPinned {
		return selected, nil
	}
	resolver, ok := c.History.(interface {
		ResolvePRSourcePosition(context.Context, domain.HistoryEvent) (domain.WorkingPosition, error)
	})
	if !ok {
		return selected, fmt.Errorf("completed PR memory resolver unavailable")
	}
	found := false
	for _, e := range history {
		matches := e.Branch == branch
		if identity != "" {
			matches = e.BranchID == identity
		}
		if !matches || e.Kind != "pr-merge" || !e.PRCompleted || e.PR == nil || e.PR.MergeSHA != selected.GitCommit || e.Source != selected.Snapshot {
			continue
		}
		p, err := resolver.ResolvePRSourcePosition(ctx, e)
		if err != nil {
			return selected, err
		}
		if p.Snapshot != selected.Snapshot || !p.MemoryPinned {
			return selected, domain.ErrHashMismatch
		}
		if found && (p.MemoryHash != selected.MemoryHash || p.MemorySource != selected.MemorySource) {
			return selected, fmt.Errorf("conflicting completed PR memory for Git %s", selected.GitCommit)
		}
		selected.MemoryHash, selected.MemorySource, selected.MemoryPinned = p.MemoryHash, p.MemorySource, true
		found = true
	}
	if !found {
		return selected, fmt.Errorf("completed PR source proof unavailable")
	}
	return selected, nil
}

func snapshotContains(snaps []domain.Snapshot, from, ancestor domain.ContentHash) bool {
	if from == "" || ancestor == "" {
		return false
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snaps))
	for _, s := range snaps {
		byID[s.ID] = s
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{from}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == ancestor {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if s, ok := byID[id]; ok {
			queue = append(queue, s.ReachabilityParents()...)
		}
	}
	return false
}

// Git's reference transaction can select an ancestor before post-merge fetches
// the exact PR receipt. Refresh only that code-move selection, never a new
// capture, a same-code manual selection, a different worktree or a future ref.
// This is local metadata reconciliation; it does not rewrite a live transcript.
func reconcileCompletedPRPosition(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil || c.List == nil || operationInProgress(cwd) {
		return nil
	}
	cas, ok := c.History.(interface {
		SelectPositionIfCurrent(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
	})
	if !ok {
		return nil
	}
	old, err := c.History.CurrentPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	branch := gitOut(cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	code := gitOut(cwd, "rev-parse", "--verify", "HEAD")
	e := old.Selection
	if branch == "" || old.Orphan || old.GitBranch() != branch || e == nil || e.Kind != "position" || e.Target != old.Snapshot || e.WorktreeID != old.WorktreeID || e.GitAfter != old.GitCommit {
		return nil
	}
	before := e.GitBefore
	selectPosition := cas.SelectPositionIfCurrent
	codeMoved := old.GitCommit != code
	if codeMoved {
		// The original reference hook may have preserved the old position while
		// the receipt was unavailable. Only replay the latest observed Git move;
		// an unrelated checkout/reset or missing reflog cannot authorize it.
		mover, ok := c.History.(inbound.ContextCodeMoveReconciler)
		if !ok || gitOut(cwd, "rev-parse", "--verify", "HEAD@{1}") != old.GitCommit {
			return nil
		}
		before = old.GitCommit
		selectPosition = mover.SelectPositionAfterCodeMove
	} else if before == "" || before == code {
		return nil
	}
	if e.RepoID != old.RepoID || e.BranchID != old.BranchID || e.Branch != old.Branch {
		return domain.ErrHashMismatch
	}
	binding, err := c.History.ResolveLocalBranch(ctx, old.RepoID, branch)
	if err != nil {
		return err
	}
	if binding.Inactive || binding.Branch != old.Branch || (binding.BranchID != "" && binding.BranchID != old.BranchID) {
		return nil
	}
	history, err := c.History.ListHistory(ctx, old.RepoID)
	if err != nil {
		return err
	}
	selected, err := completedPRSelection(code, old.Branch, old.BranchID, history)
	if err != nil {
		return err
	}
	if selected.Snapshot == "" {
		return nil
	}
	all, err := c.List.List(ctx, inbound.ListInput{RepoID: old.RepoID})
	if err != nil {
		return err
	}
	var ref domain.Ref
	for _, r := range all.Refs {
		if r.Kind == domain.RefBranch && r.Name == old.Branch && r.RepoID == old.RepoID {
			ref = r
			break
		}
	}
	if ref.Target != selected.Snapshot || ref.Target == old.SharedTarget || (ref.BranchID != "" && ref.BranchID != old.BranchID) || !snapshotContains(all.Snapshots, selected.Snapshot, old.Snapshot) {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", before, code)
	cmd.Dir = cwd
	if err := cmd.Run(); err != nil {
		var status *exec.ExitError
		if errors.As(err, &status) && status.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("verify incoming Git ancestry: %w", err)
	}
	selected, err = resolveCompletedPRMemory(ctx, c, old.Branch, old.BranchID, selected, history)
	if err != nil {
		return err
	}
	selected.RepoID, selected.WorktreeID = old.RepoID, old.WorktreeID
	selected.Branch, selected.BranchID, selected.LocalBranch = old.Branch, old.BranchID, old.LocalBranch
	selected.GitCommit = code
	// Check Git again after the potentially slow local reads. The store CAS
	// fences a capture, repin or ref movement during reconciliation.
	if gitOut(cwd, "symbolic-ref", "--quiet", "--short", "HEAD") != branch || gitOut(cwd, "rev-parse", "--verify", "HEAD") != code {
		return domain.ErrSyncConflict
	}
	if codeMoved && gitOut(cwd, "rev-parse", "--verify", "HEAD@{1}") != old.GitCommit {
		return domain.ErrSyncConflict
	}
	return selectPosition(ctx, old, selected, ref)
}
