package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocumentFindFragmentQuery interface {
	ReadDocFindFragment(context.Context, domain.ContentHash, domain.ContentHash, domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error)
}
