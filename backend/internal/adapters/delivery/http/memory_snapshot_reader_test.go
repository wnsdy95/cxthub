package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memorySnapshotHTTPBackendTrap struct {
	Backend
	scopes int
	reads  int
}

func (backend *memorySnapshotHTTPBackendTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}

func (backend *memorySnapshotHTTPBackendTrap) GetMemoryDigest(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	return domain.MemoryDigest{}, domain.ErrIntegrity
}

func (backend *memorySnapshotHTTPBackendTrap) GetMemoryObject(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	return domain.MemoryDigest{}, domain.ErrIntegrity
}
