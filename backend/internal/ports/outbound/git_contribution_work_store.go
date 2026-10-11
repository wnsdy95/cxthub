package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContributionWorkStore interface {
	ClaimGitContributionWork(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.GitContributionWork, error)
	FinishGitContributionWork(context.Context, domain.GitContributionWork) error
}
