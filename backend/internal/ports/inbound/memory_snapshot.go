package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemorySnapshotQueries interface {
	MemoryReadScope
	GetMemoryObject(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	GetMemoryDigest(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	GetMemoryProjection(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryProjection, error)
}
