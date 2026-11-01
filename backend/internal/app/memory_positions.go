package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || len(event) > 128 {
		return domain.MemoryPositions{}, domain.ErrValidation
	}
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		return domain.MemoryPositions{}, err
	}
	return queries.QueryMemoryPositions(ctx, repo, snapshot, event)
}
