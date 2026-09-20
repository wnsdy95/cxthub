package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || len(event) > 128 {
		return domain.MemoryPositions{}, domain.ErrValidation
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.MemoryPositions, error) {
		if _, err := s.meta.GetSnapshot(ctx, repo, snapshot); err != nil {
			return domain.MemoryPositions{}, err
		}
		history, err := s.ListHistory(ctx, repo)
		if err != nil {
			return domain.MemoryPositions{}, err
		}
		return domain.ResolveMemoryPositions(snapshot, event, history), nil
	})
}
