package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type GraphEvidenceReader interface {
	RepositoryReader
	GitTreeStore
	GitDeltaReader
	PRHistoryReader
	PRContributionReader
	GitContributionReader
	GitHeadReceiptReader
	GitContributionAdmissionReader
	GitContextHeadReader
}
