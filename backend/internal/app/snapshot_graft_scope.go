package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type snapshotGraftScope struct {
	work *repositoryWork
}

func (service *Service) SnapshotGraftCommands() (*SnapshotGraftCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	work := service.repositoryWork()
	if err := work.validateCompleteAuthority(); err != nil {
		return nil, err
	}
	return snapshotGraftsForWork(service.meta, work)
}

func snapshotGraftsForWork(storage SnapshotGraftStore, work *repositoryWork) (*SnapshotGraftCommands, error) {
	if work == nil {
		return nil, domain.ErrValidation
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	return NewSnapshotGraftCommands(SnapshotGraftCommandsConfig{Store: storage, Scope: &snapshotGraftScope{work: work}})
}

func (scope *snapshotGraftScope) WithinSnapshotGraftWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if scope == nil || scope.work == nil || write == nil {
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(ctx, scope.work, repo, write)
}
