package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContributionStore interface {
	PutGitContribution(context.Context, domain.GitContribution) error
	ListGitContributions(context.Context, domain.ContentHash) ([]domain.GitContribution, error)
	PutGitContributionMemory(context.Context, domain.GitContributionMemory) error
	ListGitContributionMemories(context.Context, domain.ContentHash) ([]domain.GitContributionMemory, error)
}
