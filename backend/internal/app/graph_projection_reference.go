package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphProjectionFirstPage struct {
	page      domain.RepositoryViewSourcePage
	reference domain.GraphProjectionReference
}

func (service *Service) captureGraphProjectionFirstPage(ctx context.Context, source outbound.RepositoryGraphSourceStore, job domain.GraphProjectionLease) (graphProjectionFirstPage, error) {
	scope := newProjectionWorkerScope(service.meta, service.blobs)
	generation, _ := service.meta.(outbound.RepositoryGraphSourceGenerationStore)
	page, reference, err := projectionworker.CaptureFirstPage(ctx, scope, source, generation, newProjectionWorkerReference(service.meta, scope), job)
	if err != nil {
		return graphProjectionFirstPage{}, err
	}
	return graphProjectionFirstPage{page: page, reference: reference}, nil
}

func (service *Service) graphQualificationReference(ctx context.Context, repo domain.ContentHash) (domain.ContentHash, error) {
	scope := newProjectionWorkerScope(service.meta, service.blobs)
	return newProjectionWorkerReference(service.meta, scope).Reference(ctx, repo)
}
