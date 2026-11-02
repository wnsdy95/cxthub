package repositoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type GraphScope interface {
	ReadGraph(context.Context, domain.ContentHash, func(context.Context) error) error
	RepairGraph(context.Context, domain.ContentHash, func(context.Context) error) error
	GraphRepairAllowed(context.Context) bool
}

type GraphResources interface {
	AcquireBuild(context.Context) (context.Context, func(), error)
}

type PublishedGraphReader interface {
	ReadQualifiedGraphProjection(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
}

type ServingGraphRepair interface {
	RequestGraphServingRepair(context.Context, domain.GraphProjectionCorruption) error
}

type ProjectionGraphRepair interface {
	RequestGraphProjectionRepair(context.Context, domain.GraphProjectionCorruption) error
}

type PublishedGraphConfig struct {
	Profile           Profile
	Scope             GraphScope
	Resources         GraphResources
	Reader            PublishedGraphReader
	ServingRepairs    ServingGraphRepair
	ProjectionRepairs ProjectionGraphRepair
	Repos             []domain.ContentHash
}

type PublishedGraphQueries struct {
	scope             GraphScope
	resources         GraphResources
	reader            PublishedGraphReader
	servingRepairs    ServingGraphRepair
	projectionRepairs ProjectionGraphRepair
	repos             map[domain.ContentHash]bool
}

var _ inbound.RepositoryGraphQueries = (*PublishedGraphQueries)(nil)

func NewPublishedGraph(config PublishedGraphConfig) (*PublishedGraphQueries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != PinnedSnapshots {
		return nil, fmt.Errorf("%w: graph query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Resources, config.Reader, config.ServingRepairs, config.ProjectionRepairs} {
		if absent(required) {
			return nil, fmt.Errorf("%w: published graph query dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	queries := &PublishedGraphQueries{scope: config.Scope, resources: config.Resources, reader: config.Reader,
		servingRepairs: config.ServingRepairs, projectionRepairs: config.ProjectionRepairs, repos: make(map[domain.ContentHash]bool, len(config.Repos))}
	for _, repo := range config.Repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		queries.repos[repo] = true
	}
	return queries, nil
}
