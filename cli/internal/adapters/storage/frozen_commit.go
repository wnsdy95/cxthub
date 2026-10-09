package storage

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func (s *FileStore) CommitFrozenSnapshotIfCurrent(ctx context.Context, ref domain.Ref, expected domain.ContentHash, original, next domain.WorkingPosition, event *domain.HistoryEvent) error {
	if s.gitCommit != next.GitCommit || s.gitBranch != next.GitBranch() {
		return domain.ErrSelectionChanged
	}
	return s.CommitWorkingSnapshotIfCurrent(ctx, ref, expected, original, next, event)
}
