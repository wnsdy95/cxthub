package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryProjectionReader interface {
	MemoryObjectReader
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
}

type MemoryObjectReader interface {
	GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
}

type MemoryMetadataReader interface {
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error)
}

type MemoryQueryReader interface {
	MemoryProjectionReader
	MemoryMetadataReader
	PRContributionMemoryReader
	GitContributionMemoryReader
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}
