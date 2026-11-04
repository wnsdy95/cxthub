package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) checkContextWrite(ctx context.Context, repoID domain.ContentHash, next domain.Ref) error {
	return s.repositoryRefs().checkContextWrite(ctx, repoID, next)
}

func (s *Service) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	return s.branchOperations().EnableContextProtocol(ctx, repo)
}
