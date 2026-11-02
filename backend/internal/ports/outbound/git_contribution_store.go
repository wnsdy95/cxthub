package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContributionReader interface {
	ListGitContributions(context.Context, domain.ContentHash) ([]domain.GitContribution, error)
}

type GitContributionStore interface {
	GitContributionReader
	GitContributionMemoryReader
	PutGitContribution(context.Context, domain.GitContribution) error
	PutGitContributionMemory(context.Context, domain.GitContributionMemory) error
}

type GitContributionMemoryReader interface {
	ListGitContributionMemories(context.Context, domain.ContentHash) ([]domain.GitContributionMemory, error)
}
