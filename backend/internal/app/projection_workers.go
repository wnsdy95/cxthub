package app

import (
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ProjectionWorkersConfig struct {
	Metadata                  outbound.MetadataStore
	Blobs                     outbound.BlobStore
	Profile                   projectionworker.Profile
	PendingEnabled            bool
	GraphEnabled              bool
	QualificationRepositories []domain.ContentHash
	QualificationNext         *atomic.Uint64
}

type ProjectionWorkers struct {
	Pending       *projectionworker.Pending
	Graph         *projectionworker.Graph
	Qualification *projectionworker.Qualification
}

func NewProjectionWorkers(config ProjectionWorkersConfig) (*ProjectionWorkers, error) {
	if config.Profile != projectionworker.Compatibility && config.Profile != projectionworker.Complete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(config.Blobs) {
		return nil, domain.ErrValidation
	}
	scope := newProjectionWorkerScope(config.Metadata, config.Blobs)
	for _, optional := range []any{scope.transactions, scope.presence, scope.state} {
		if optional != nil && !rootAdapterPresent(optional) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	if (config.PendingEnabled || config.GraphEnabled || len(config.QualificationRepositories) != 0) && config.Profile == projectionworker.Complete &&
		(scope.transactions == nil || scope.presence == nil || scope.state == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	workers := &ProjectionWorkers{}
	pending, _ := config.Metadata.(outbound.PendingProjectionStore)
	if config.Profile == projectionworker.Compatibility || pending != nil || config.PendingEnabled {
		worker, err := projectionPending(config, scope)
		if err != nil {
			return nil, err
		}
		workers.Pending = worker
	}
	graph, _ := config.Metadata.(outbound.GraphProjectionStore)
	if config.Profile == projectionworker.Compatibility || graph != nil || config.GraphEnabled {
		worker, err := projectionGraph(config, scope)
		if err != nil {
			return nil, err
		}
		workers.Graph = worker
	}
	qualification, _ := config.Metadata.(outbound.GraphQualificationStore)
	if config.Profile == projectionworker.Compatibility || qualification != nil || len(config.QualificationRepositories) != 0 {
		worker, err := projectionQualification(config, scope)
		if err != nil {
			return nil, err
		}
		workers.Qualification = worker
	}
	return workers, nil
}

func projectionPending(config ProjectionWorkersConfig, scope *projectionWorkerScope) (*projectionworker.Pending, error) {
	jobs, _ := config.Metadata.(outbound.PendingProjectionStore)
	source, _ := config.Metadata.(outbound.RepositoryViewSourceStore)
	return projectionworker.NewPending(projectionworker.PendingConfig{
		Profile: config.Profile, Scope: scope, Jobs: jobs, Source: source, Enabled: config.PendingEnabled,
	})
}

func projectionGraph(config ProjectionWorkersConfig, scope *projectionWorkerScope) (*projectionworker.Graph, error) {
	jobs, _ := config.Metadata.(outbound.GraphProjectionStore)
	source, _ := config.Metadata.(outbound.RepositoryGraphSourceStore)
	factory, _ := config.Metadata.(outbound.GraphSourceReaderFactory)
	references, _ := config.Metadata.(outbound.GraphProjectionReferenceStore)
	generations, _ := config.Metadata.(outbound.RepositoryGraphSourceGenerationStore)
	reference := newProjectionWorkerReference(config.Metadata, scope)
	if config.Profile == projectionworker.Complete && rootAdapterPresent(references) &&
		(!rootAdapterPresent(reference.budget) || !rootAdapterPresent(reference.history)) {
		return nil, domain.ErrCatalogUnsupported
	}
	return projectionworker.NewGraph(projectionworker.GraphConfig{
		Profile: config.Profile, Scope: scope, Jobs: jobs, Source: source, Factory: factory,
		References: references, Generations: generations,
		Reference: reference, Enabled: config.GraphEnabled,
	})
}

func projectionQualification(config ProjectionWorkersConfig, scope *projectionWorkerScope) (*projectionworker.Qualification, error) {
	jobs, _ := config.Metadata.(outbound.GraphQualificationStore)
	projections, _ := config.Metadata.(outbound.GraphProjectionStore)
	repairs, _ := config.Metadata.(outbound.GraphProjectionRepairStore)
	generations, _ := config.Metadata.(outbound.RepositoryGraphSourceGenerationStore)
	reference := newProjectionWorkerReference(config.Metadata, scope)
	if config.Profile == projectionworker.Complete && len(config.QualificationRepositories) != 0 && !rootAdapterPresent(reference.history) {
		return nil, domain.ErrCatalogUnsupported
	}
	return projectionworker.NewQualification(projectionworker.QualificationConfig{
		Profile: config.Profile, Scope: scope, Jobs: jobs, Projections: projections, Repairs: repairs,
		Generations: generations, Reference: reference,
		Repositories: config.QualificationRepositories, Next: config.QualificationNext,
	})
}

func (service *Service) ProjectionWorkersForProfile(profile projectionworker.Profile) (*ProjectionWorkers, error) {
	return NewProjectionWorkers(service.projectionWorkersConfig(profile))
}

func (service *Service) projectionWorkersConfig(profile projectionworker.Profile) ProjectionWorkersConfig {
	return ProjectionWorkersConfig{
		Metadata: service.meta, Blobs: service.blobs, Profile: profile,
		PendingEnabled:            len(service.pendingProjectionRepos) != 0,
		GraphEnabled:              len(service.graphProjectionRepos) != 0,
		QualificationRepositories: service.graphQualificationRepos, QualificationNext: &service.graphQualificationNext,
	}
}
