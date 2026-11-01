package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryDigestScope struct {
	work *repositoryWork
}

func (service *Service) MemoryDigestCommands() (*MemoryDigestCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	work := service.repositoryWork()
	if err := work.validateCompleteAuthority(); err != nil {
		return nil, err
	}
	return memoryDigestsForWork(service.blobs, service.meta, work)
}

func memoryDigestsForWork(blobs MemoryDigestBlobStore, snapshots MemoryDigestSnapshotStore, work *repositoryWork) (*MemoryDigestCommands, error) {
	if work == nil {
		return nil, domain.ErrValidation
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	return NewMemoryDigestCommands(MemoryDigestCommandsConfig{Blobs: blobs, Snapshots: snapshots, Scope: &memoryDigestScope{work: work}})
}

func (scope *memoryDigestScope) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	if scope == nil || scope.work == nil || write == nil {
		return "", domain.ErrValidation
	}
	return repositoryWriteWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), scope.work, repo, write)
}

func (scope *memoryDigestScope) WithinMemoryAttachmentWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if scope == nil || scope.work == nil || write == nil {
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(ctx, scope.work, repo, write)
}

func (service *Service) putMemoryDigest(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest, expected domain.ContentHash, causal bool) (domain.ContentHash, error) {
	commands, err := memoryDigestsForWork(service.blobs, service.meta, service.repositoryWork())
	if err != nil {
		return "", err
	}
	return commands.putMemoryDigest(ctx, repo, digest, expected, causal)
}
