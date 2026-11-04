package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) PreviewJoin(ctx context.Context, in inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	return s.branchOperations().PreviewJoin(ctx, in)
}

func (s *Service) ConfirmJoin(ctx context.Context, in inbound.ConfirmJoinInput) (inbound.JoinOutput, error) {
	return s.branchOperations().ConfirmJoin(ctx, in)
}

func (s *Service) authorizeJoin(ctx context.Context, repoID domain.ContentHash, actor string, lock bool) error {
	commands := s.branchOperations()
	if err := commands.ready(); err != nil {
		return err
	}
	return commands.access.AuthorizeJoin(ctx, repoID, actor, lock)
}
