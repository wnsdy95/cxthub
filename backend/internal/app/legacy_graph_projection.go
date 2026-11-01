package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyGraphProjectionScope struct {
	state outbound.RepositoryTransactionState
}

func (scope *legacyGraphProjectionScope) IsPinnedRead(ctx context.Context) bool {
	_, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	return !writing && scope.state != nil && scope.state.InReadOnlyTransaction(ctx)
}

func (*legacyGraphProjectionScope) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	return acquireGraphBuild(ctx)
}

func (service *Service) legacyGraphProjection() (*repositoryquery.LegacyProjection, error) {
	state, _ := service.meta.(outbound.RepositoryTransactionState)
	scope := &legacyGraphProjectionScope{state: state}
	reader := service.graphEvidenceReader()
	contributions, err := service.evidenceQueries()
	if err != nil {
		return nil, err
	}
	generations, _ := service.meta.(repositoryquery.GraphGenerationReader)
	trees, _ := reader.(evidencequery.GitTreeReader)
	deltas, _ := reader.(evidencequery.GitDeltaReader)
	return repositoryquery.NewLegacyProjection(repositoryquery.LegacyProjectionConfig{
		Scope: scope, Resources: scope, Metadata: reader, Contributions: contributions,
		Generations: generations, Trees: trees, Deltas: deltas, Graphs: &service.graphCache, Branches: &service.branchCache,
		Captured: service.projectionEvidence != nil, RequireComplete: service.requireCompleteMemory,
	})
}

func capturedGraphProjection(reader outbound.GraphEvidenceReader) (*repositoryquery.LegacyProjection, error) {
	return repositoryquery.NewCapturedProjection(reader, &legacyGraphProjectionScope{})
}
