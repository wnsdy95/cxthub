package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type MemoryProjectionSource = outbound.MemoryProjectionReader

type serviceProjectionSource struct {
	outbound.MemoryMetadataReader
	outbound.MemoryObjectReader
}

const maxProjectionSnapshots = memoryquery.MaxProjectionSnapshots
const maxProjectionEdges = memoryquery.MaxProjectionEdges
const maxProjectionMemoryBytes = memoryquery.MaxProjectionMemoryBytes
const maxProjectionCacheBytes = memoryquery.MaxProjectionCacheBytes

func (service *Service) getMemoryProjection(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	return memoryquery.ProjectMemory(ctx, service.memoryProjectionReader(), repo, snapshot)
}

func ProjectMemory(ctx context.Context, source MemoryProjectionSource, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	return memoryquery.ProjectMemory(ctx, source, repo, snapshot)
}
