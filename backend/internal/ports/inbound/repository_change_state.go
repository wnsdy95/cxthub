package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryChangeQuery interface {
	RepositoryChangeState(context.Context, domain.ContentHash) (domain.RepositoryChangeState, error)
}
