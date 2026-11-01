package app

import (
	"context"
	"log"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigurePendingProjections(ctx context.Context, repos []domain.ContentHash) error {
	if len(repos) == 0 {
		service.pendingProjectionRepos = nil
		return nil
	}
	storage, ok := service.meta.(outbound.PendingProjectionStore)
	if !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.RepositoryViewSourceStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
	}
	configured := make(map[domain.ContentHash]bool, len(repos))
	for _, repo := range repos {
		if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
			return err
		}
		configured[repo] = true
	}
	service.pendingProjectionRepos = configured
	return nil
}

func (service *Service) ProcessPendingProjections(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	worker, err := projectionPending(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		return err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) runPendingProjection(ctx context.Context, storage outbound.PendingProjectionStore, source outbound.RepositoryViewSourceStore, job domain.PendingProjectionLease) error {
	worker, err := projectionworker.NewPending(projectionworker.PendingConfig{
		Profile: projectionworker.Compatibility, Scope: newProjectionWorkerScope(service.meta, service.blobs), Jobs: storage, Source: source,
	})
	if err != nil {
		return err
	}
	return worker.RunLease(ctx, job)
}

func (service *Service) RunPendingProjectionWorker(ctx context.Context) {
	if len(service.pendingProjectionRepos) == 0 {
		return
	}
	worker, err := projectionPending(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		log.Printf("pending projection: %v", err)
		return
	}
	worker.Run(ctx)
}
