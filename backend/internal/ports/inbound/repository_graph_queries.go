package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryGraphQueryProfile = RepositoryStateQueryProfile

const (
	RepositoryGraphCompatibility   = RepositoryStateCompatibility
	RepositoryGraphPinnedSnapshots = RepositoryStatePinnedSnapshots
)

type RepositoryGraphQueries interface {
	GraphStateQuery
	GetRepositoryView(context.Context, domain.ContentHash) (domain.RepositoryView, error)
	GetPendingView(context.Context, domain.ContentHash) (domain.PendingView, error)
}
