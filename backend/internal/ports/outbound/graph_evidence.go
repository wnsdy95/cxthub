package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}
