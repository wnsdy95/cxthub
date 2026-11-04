package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryStateQueryProfile uint8

const (
	RepositoryStateCompatibility RepositoryStateQueryProfile = iota + 1
	RepositoryStatePinnedSnapshots
)

type RepositoryStateQueries interface {
	RepositoryChangeQuery
	PendingStateQuery
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}
