package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocumentQueryProfile uint8

const (
	DocumentQueryCompatibility DocumentQueryProfile = iota + 1
	DocumentQueryVerifiedRoots
)

type DocumentEventQueries interface {
	ReadDocFragments(context.Context, domain.ContentHash, domain.ContentHash, int, int, int, int) (domain.DocFragmentPage, error)
	SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error)
}

type DocumentViewQueries interface {
	ReadDocViewEvents(context.Context, domain.ContentHash, domain.ContentHash, int, int, string) (domain.DocViewPage, error)
	ReadDocViewFragment(context.Context, domain.ContentHash, domain.ContentHash, int, int, int) (domain.DocViewFragment, error)
}

type DocumentQueries interface {
	DocumentEventQueries
	DocumentViewQueries
}
