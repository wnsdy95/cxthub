package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitChangeQuery interface {
	Get(context.Context, domain.ContentHash, string) (domain.GitChangeJob, error)
	List(context.Context, domain.ContentHash, string, int) (domain.GitChangePage, error)
}

type GitChanges interface {
	GitChangeQuery
	Submit(context.Context, domain.ContentHash, domain.GitChangeRequest) (domain.GitChangeJob, error)
	Retry(context.Context, domain.ContentHash, string) error
}
