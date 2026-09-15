package storage

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func (s *FileStore) RenameWorkingBranch(ctx context.Context, repo, from, to string) error {
	return s.withRefMutationLock(ctx, func() error { return s.renameWorkingPositions(repo, from, to, "", true) })
}

// A Git branch rename changes the name in every attached worktree, not the
// code/context each worktree selected. Retry can finish any partial projection.
func (s *FileStore) renameWorkingPositions(repo, from, to, identity string, canonical bool) error {
	dir := filepath.Join(s.storeDir(), "worktrees")
	if err := validateCxtDir(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		owner := *s
		owner.worktreeID = entry.Name()
		p, err := owner.readPosition()
		if err == domain.ErrNotFound {
			continue
		}
		if err != nil {
			return err
		}
		if p.RepoID != repo || p.GitBranch() != from || (identity != "" && p.BranchID != identity) {
			continue
		}
		if canonical {
			p.Branch = to
			p.LocalBranch = ""
		} else {
			p.LocalBranch = to
			if to == p.Branch {
				p.LocalBranch = ""
			}
		}
		p.Selection = nil
		if err := owner.writePosition(p); err != nil {
			return err
		}
	}
	return nil
}
