package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryIntegration = memoryquery.Assessment

func newMemoryIntegration(service *Service, evidence *codeEvidence, repo domain.ContentHash, history []domain.HistoryEvent) (*memoryIntegration, error) {
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		return nil, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return nil, err
	}
	return queries.NewAssessment(context.Background(), repo, history, owner)
}

func resolveEffectiveMemoryPage(ctx context.Context, resolver *memoryIntegration, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem, offset, limit int) (domain.EffectiveMemoryPage, error) {
	return resolver.ResolvePage(ctx, out, items, offset, limit)
}

func effectiveMemoryDeliveryHash(ctx context.Context, resolver *memoryIntegration, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem) (domain.ContentHash, error) {
	return resolver.DeliveryHash(ctx, out, items)
}
