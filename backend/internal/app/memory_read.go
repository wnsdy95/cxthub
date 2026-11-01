package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func BuildMemoryRead(ctx context.Context, input domain.MemoryCaptureBundle, factory outbound.GraphSourceReaderFactory) (domain.MemoryReadBundle, error) {
	captured, err := RestoreCapturedEffectiveMemory(ctx, input, factory)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	return buildCapturedMemoryRead(ctx, captured, input.ID)
}

func buildCapturedMemoryRead(ctx context.Context, captured *CapturedEffectiveMemory, captureID domain.ContentHash) (domain.MemoryReadBundle, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryReadBundle{}, err
	}
	if captured == nil || captured.reader == nil || captured.evidence == nil || captured.generation.Validate() != nil ||
		captured.reader.repo != captured.generation.Graph.RepoID || captured.graph.Checkpoint() != captured.generation.Graph ||
		domain.ValidateContentHash(captureID) != nil {
		return domain.MemoryReadBundle{}, domain.ErrIntegrity
	}
	queries, err := newFrozenMemoryQueries(captured.reader, captured.evidence, &captured.generation, true)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	var dependencies []domain.MemoryReadDependency
	for _, dependency := range captured.Dependencies() {
		dependencies = append(dependencies, domain.MemoryReadDependency{Hash: dependency.Hash, Available: dependency.Available})
	}
	return memoryquery.BuildRead(ctx, queries, captured.generation.Graph.RepoID, captureID, captured.generation, captured.selection, captured.content, dependencies)
}

func ReadMemoryReadPage(ctx context.Context, reader outbound.MemoryReadRangeStore, repo, identity domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	return memoryquery.ReadPage(ctx, reader, repo, identity, request)
}
