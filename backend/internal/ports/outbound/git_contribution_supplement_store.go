package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContributionSupplementStore interface {
	ClaimGitContributionSupplement(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.GitContributionSupplement, error)
	FinishGitContributionSupplement(context.Context, domain.GitContributionSupplement) error
	GetGitContributionSupplementProof(context.Context, domain.ContentHash, string) (domain.GitContributionProof, error)
	FindGitContributionSupplementMemory(context.Context, domain.ContentHash, string, string) (domain.GitContributionMemory, error)
}
