package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryBootstrapQuery interface {
	GetRepositoryBootstrap(context.Context, domain.ContentHash, string) (domain.RepositoryBootstrap, error)
}
