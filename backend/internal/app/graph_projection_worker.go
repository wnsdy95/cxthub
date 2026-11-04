package app

import (
	"context"
	"log"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigureGraphProjections(ctx context.Context, repos []domain.ContentHash) error {
	if len(repos) == 0 {
		service.graphProjectionRepos = nil
		return nil
	}
	storage, ok := service.meta.(outbound.GraphProjectionStore)
	if !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.RepositoryGraphSourceStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.GraphSourceReaderFactory); !ok {
		return domain.ErrCatalogUnsupported
	}
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
	}
	configured := make(map[domain.ContentHash]bool, len(repos))
	for _, repo := range repos {
		if configured[repo] {
			continue
		}
		if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
			return err
		}
		configured[repo] = true
	}
	service.graphProjectionRepos = configured
	return nil
}

func (service *Service) ProcessGraphProjections(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	worker, err := projectionGraph(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		return err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) runGraphProjection(ctx context.Context, storage outbound.GraphProjectionStore, source outbound.RepositoryGraphSourceStore, factory outbound.GraphSourceReaderFactory, job domain.GraphProjectionLease) error {
	scope := newProjectionWorkerScope(service.meta, service.blobs)
	references, _ := storage.(outbound.GraphProjectionReferenceStore)
	generations, _ := service.meta.(outbound.RepositoryGraphSourceGenerationStore)
	worker, err := projectionworker.NewGraph(projectionworker.GraphConfig{
		Profile: projectionworker.Compatibility, Scope: scope, Jobs: storage, Source: source, Factory: factory,
		References: references, Generations: generations, Reference: newProjectionWorkerReference(service.meta, scope),
	})
	if err != nil {
		return err
	}
	return worker.RunLease(ctx, job)
}

func validateGraphProjectionPage(ctx context.Context, job domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	return projectionworker.ValidateGraphProjectionPage(ctx, job, page)
}

func projectGraphProjection(ctx context.Context, storage outbound.GraphProjectionStore, factory outbound.GraphSourceReaderFactory, job domain.GraphProjectionLease) error {
	return projectionworker.ProjectGraphProjection(ctx, storage, factory, job, &legacyGraphProjectionScope{})
}

func (service *Service) RunGraphProjectionWorker(ctx context.Context) {
	if len(service.graphProjectionRepos) == 0 {
		return
	}
	worker, err := projectionGraph(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		log.Printf("graph projection: %v", err)
		return
	}
	worker.Run(ctx)
}
