package repositoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ProjectionCacheScope interface {
	IsPinnedRead(context.Context) bool
}

type GraphGenerationReader interface {
	ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error)
}

type ProjectionMetadataReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type ContributionEvidence interface {
	VerifiedPR(context.Context, domain.ContentHash, []domain.HistoryEvent) ([]evidencequery.VerifiedPRContribution, error)
	VerifiedGit(context.Context, domain.ContentHash, []domain.HistoryEvent) ([]evidencequery.VerifiedGitContribution, error)
}

type LegacyProjectionConfig struct {
	Scope           ProjectionCacheScope
	Resources       GraphResources
	Metadata        ProjectionMetadataReader
	Contributions   ContributionEvidence
	Generations     GraphGenerationReader
	Trees           evidencequery.GitTreeReader
	Deltas          evidencequery.GitDeltaReader
	Graphs          *GraphCache
	Branches        *BranchCache
	Captured        bool
	RequireComplete bool
}

type LegacyProjection struct {
	scope           ProjectionCacheScope
	resources       GraphResources
	metadata        ProjectionMetadataReader
	contributions   ContributionEvidence
	generations     GraphGenerationReader
	trees           evidencequery.GitTreeReader
	deltas          evidencequery.GitDeltaReader
	graphs          *GraphCache
	branches        *BranchCache
	captured        bool
	requireComplete bool
}

func NewLegacyProjection(config LegacyProjectionConfig) (*LegacyProjection, error) {
	for _, required := range []any{config.Scope, config.Resources, config.Metadata, config.Contributions, config.Graphs, config.Branches} {
		if absent(required) {
			return nil, fmt.Errorf("%w: legacy graph projection dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	for _, optional := range []any{config.Generations, config.Trees, config.Deltas} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: declared legacy graph projection dependency is nil", domain.ErrCatalogUnsupported)
		}
	}
	return &LegacyProjection{scope: config.Scope, resources: config.Resources, metadata: config.Metadata,
		contributions: config.Contributions, generations: config.Generations, trees: config.Trees, deltas: config.Deltas,
		graphs: config.Graphs, branches: config.Branches, captured: config.Captured, requireComplete: config.RequireComplete}, nil
}
