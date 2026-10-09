package storage

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func (s *FileStore) CommitFrozenSnapshotIfCurrent(ctx context.Context, ref domain.Ref, expected domain.ContentHash, original, next domain.WorkingPosition, event *domain.HistoryEvent) error {
	if !domain.ValidGitOID(next.GitCommit) || domain.ValidateBranchName(next.GitBranch()) != nil || next.WorktreeID != s.worktreeID {
		return domain.ErrSelectionChanged
	}
	// ApplyFrozen verifies the live Git tuple before calling this port. The
	// worker may predate that commit, so its startup labels are not a live Git
	// fence. Bind a private view to the verified job rather than letting the
	// common transaction replace its code with the worker's startup SHA.
	// The full original position and shared ref still CAS under the same lock.
	view := *s
	view.gitCommit, view.gitBranch = next.GitCommit, next.GitBranch()
	return view.CommitWorkingSnapshotIfCurrent(ctx, ref, expected, original, next, event)
}
