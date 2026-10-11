package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitHubPRHistory interface {
	PullRequestHeadHistory(context.Context, int64, domain.GitHubRepository, int, string) (domain.GitHubPRHistoryPage, error)
}
