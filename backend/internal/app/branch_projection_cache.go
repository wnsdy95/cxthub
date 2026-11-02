package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func validProjectionGraphGeneration(repo domain.ContentHash, generation domain.RepositoryViewSourceCheckpoint) bool {
	return repositoryquery.ValidProjectionGraphGeneration(repo, generation)
}

func projectionGenerationUnavailable(err error) bool {
	return repositoryquery.ProjectionGenerationUnavailable(err)
}

func (service *Service) projectionGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, bool, error) {
	state, _ := service.meta.(outbound.RepositoryTransactionState)
	generations, _ := service.meta.(repositoryquery.GraphGenerationReader)
	return repositoryquery.ReadProjectionGraphSourceGeneration(ctx, repo, &legacyGraphProjectionScope{state: state}, generations, service.projectionEvidence != nil)
}
