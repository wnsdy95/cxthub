package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) memorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, *domain.RepositoryMemorySourceGeneration, error) {
	return readMemorySourceGeneration(ctx, service.meta, repo, service.projectionMemory, service.projectionEvidence, service.projectionMemorySource)
}

func readMemorySourceGeneration(ctx context.Context, metadata outbound.MetadataStore, repo domain.ContentHash, memory outbound.MemoryQueryReader, evidence outbound.GraphEvidenceReader, frozen *domain.RepositoryMemorySourceGeneration) (domain.RepositoryViewSourceCheckpoint, *domain.RepositoryMemorySourceGeneration, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, err
	}
	if memory != nil {
		generation := frozen
		if generation == nil || generation.Validate() != nil || generation.Graph.RepoID != repo || evidence == nil {
			return domain.RepositoryViewSourceCheckpoint{}, nil, domain.ErrIntegrity
		}
		return generation.Graph, generation, nil
	}
	state, _ := metadata.(outbound.RepositoryTransactionState)
	graphs, _ := metadata.(repositoryquery.GraphGenerationReader)
	graph, complete, err := repositoryquery.ReadProjectionGraphSourceGeneration(ctx, repo, &legacyGraphProjectionScope{state: state}, graphs, evidence != nil)
	if err != nil || !complete {
		return graph, nil, err
	}
	storage, supported := metadata.(outbound.RepositoryMemorySourceGenerationStore)
	if !supported {
		return graph, nil, nil
	}
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if canceled := ctx.Err(); canceled != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, canceled
	}
	if err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, err
	}
	if generation.Validate() != nil || generation.Graph != graph {
		return domain.RepositoryViewSourceCheckpoint{}, nil, domain.ErrIntegrity
	}
	return graph, &generation, nil
}
