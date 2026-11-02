package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PRContributionReader interface {
	ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error)
}

type PRContributionStore interface {
	PRContributionReader
	PRContributionMemoryReader
	PutPRContribution(context.Context, domain.PRContribution) error
	PutPRContributionMemory(context.Context, domain.PRContributionMemory) error
}

type PRContributionMemoryReader interface {
	ListPRContributionMemories(context.Context, domain.ContentHash) ([]domain.PRContributionMemory, error)
}
