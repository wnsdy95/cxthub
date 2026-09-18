package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryRevisions interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
	AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error
}

// Evidence updates do not change graph or live-capture payloads.
type EvidenceRevisions interface {
	AdvanceEvidenceRevision(context.Context, domain.ContentHash) error
}
