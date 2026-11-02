package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) promoteBoundPRInTransaction(ctx context.Context, repoID domain.ContentHash, pr domain.PullRequestMerge, expectedBaseID string) (inbound.UpdateRefOutput, error) {
	return s.prPromotion().promoteBoundPRInTransaction(ctx, repoID, pr, expectedBaseID)
}

func (s *Service) completePRPromotion(ctx context.Context, receipt domain.HistoryEvent, before domain.ContentHash, out inbound.UpdateRefOutput, verified historyVerification) (inbound.UpdateRefOutput, error) {
	return s.prPromotion().completePRPromotion(ctx, receipt, before, out, verified)
}

func (s *Service) resolvePRSource(ctx context.Context, repoID domain.ContentHash, pr domain.PullRequestMerge, events []domain.HistoryEvent) (domain.ContentHash, string, error) {
	return s.prPromotion().resolvePRSource(ctx, repoID, pr, events)
}

func (s *Service) promoteHistoricalPRContribution(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, bool, error) {
	return s.prPromotion().promoteHistoricalPRContribution(ctx, repo, pr, baseID)
}
