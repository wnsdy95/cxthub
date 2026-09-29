package storage

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ReadWorkingPosition reads the atomically published cursor as-is. A pending
// journal is for an explicit writer/recovery operation, never a query to replay.
func (s *FileStore) ReadWorkingPosition(ctx context.Context, repoID string) (domain.WorkingPosition, error) {
	if err := ctx.Err(); err != nil {
		return domain.WorkingPosition{}, err
	}
	p, err := s.readPosition()
	if err != nil {
		return domain.WorkingPosition{}, err
	}
	if repoID != "" && p.RepoID != repoID {
		return domain.WorkingPosition{}, domain.ErrHashMismatch
	}
	if p.WorktreeID != s.worktreeID {
		return domain.WorkingPosition{}, domain.ErrHashMismatch
	}
	if p.GitBranch() != s.gitBranch {
		return domain.WorkingPosition{}, domain.ErrNotFound
	}
	return p, nil
}

var _ outbound.WorkingPositionQueryReader = (*FileStore)(nil)
