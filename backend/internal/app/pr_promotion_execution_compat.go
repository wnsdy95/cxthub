package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) runPRJob(ctx context.Context, j domain.PRPromotionJob) (inbound.UpdateRefOutput, error) {
	return s.prPromotion().runPRJob(ctx, j)
}

func (s *Service) ProcessPRPromotions(ctx context.Context, limit int) error {
	return s.prPromotion().ProcessPRPromotions(ctx, limit)
}

func (s *Service) RunPRPromotionWorker(ctx context.Context) {
	s.prPromotion().RunPRPromotionWorker(ctx)
}
