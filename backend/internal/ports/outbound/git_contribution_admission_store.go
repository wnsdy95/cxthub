package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContributionAdmissionReader interface {
	GetGitContributionAdmission(context.Context, domain.ContentHash, string) (domain.GitContributionAdmission, error)
}

type GitContributionAdmissionStore interface {
	GitContributionAdmissionReader
	FenceGitHeadRead(context.Context, domain.ContentHash, string) (int64, error)
	PutGitContributionAdmission(context.Context, domain.GitContributionAdmission) error
	FindGitContributionAdmission(context.Context, domain.ContentHash, string) (domain.GitContributionAdmission, error)
	ListGitContributionAdmissions(context.Context, domain.ContentHash, string, string, int) ([]domain.GitContributionAdmission, error)
}
