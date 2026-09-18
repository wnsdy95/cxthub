package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryRevisions interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
	AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error
}
