package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PRContributionStore interface {
	PutPRContribution(context.Context, domain.PRContribution) error
	ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error)
	PutPRContributionMemory(context.Context, domain.PRContributionMemory) error
	ListPRContributionMemories(context.Context, domain.ContentHash) ([]domain.PRContributionMemory, error)
}
