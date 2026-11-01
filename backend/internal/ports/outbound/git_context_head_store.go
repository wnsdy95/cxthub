package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitContextHeadReader interface {
	GetGitContextHead(context.Context, domain.ContentHash, string) (domain.GitContextHead, error)
	ListGitContributionProofs(context.Context, domain.ContentHash) ([]domain.GitContributionProof, error)
}

type GitContextHeadStore interface {
	GitContextHeadReader
	PutGitContextHeads(context.Context, domain.GitHeadReceipt, []domain.GitContextHead) error
	ListGitContextHeads(context.Context, domain.ContentHash, string, string, int64, int) ([]domain.GitContextHead, error)
	PutGitContributionProof(context.Context, domain.GitContributionProof) error
	FindGitContributionProof(context.Context, domain.ContentHash, string) (domain.GitContributionProof, error)
}
