package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContextHeadStore interface {
	PutGitContextHeads(context.Context, domain.GitHeadReceipt, []domain.GitContextHead) error
	GetGitContextHead(context.Context, domain.ContentHash, string) (domain.GitContextHead, error)
	ListGitContextHeads(context.Context, domain.ContentHash, string, string, int64, int) ([]domain.GitContextHead, error)
	PutGitContributionProof(context.Context, domain.GitContributionProof) error
	ListGitContributionProofs(context.Context, domain.ContentHash) ([]domain.GitContributionProof, error)
	FindGitContributionProof(context.Context, domain.ContentHash, string) (domain.GitContributionProof, error)
}
