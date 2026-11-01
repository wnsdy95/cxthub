package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type MemoryPreparationProfile string

const (
	MemoryPreparationCompatibility MemoryPreparationProfile = "compatibility"
	MemoryPreparationComplete      MemoryPreparationProfile = "complete"
)

type MemoryPreparationResourceConfig struct {
	Metadata     outbound.MetadataStore
	Blobs        outbound.BlobStore
	Repositories outbound.RepositoryStore
}

type MemoryPreparationResources struct {
	meta                   outbound.MetadataStore
	blobs                  outbound.BlobStore
	work                   *repositoryWork
	scope                  *memorySnapshotScope
	delivery               *memoryquery.DeliveryCache
	projectionMemory       outbound.MemoryQueryReader
	projectionEvidence     outbound.GraphEvidenceReader
	projectionMemorySource *domain.RepositoryMemorySourceGeneration
}

type MemoryPreparationWorkerConfig struct {
	Profile      MemoryPreparationProfile
	Resources    *MemoryPreparationResources
	Repositories []domain.ContentHash
}

type MemoryPreparationWorkers struct {
	*MemoryPreparationResources
	memoryReadRepos []domain.ContentHash
}

func NewMemoryPreparationResources(config MemoryPreparationResourceConfig) (*MemoryPreparationResources, error) {
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(config.Blobs) ||
		config.Repositories != nil && !rootAdapterPresent(config.Repositories) {
		return nil, domain.ErrValidation
	}
	work := newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))
	transactions, _ := config.Metadata.(documentQuerySnapshots)
	resources := newMemoryResourceScope(config.Metadata, &memoryReadOwner{})
	return &MemoryPreparationResources{meta: config.Metadata, blobs: config.Blobs, work: work,
		scope: &memorySnapshotScope{resources: resources, repository: &repositoryQueryScope{
			metadata: config.Metadata, transactions: transactions, supported: work.supported,
		}}, delivery: &memoryquery.DeliveryCache{}}, nil
}

func NewMemoryPreparationWorkers(config MemoryPreparationWorkerConfig) (*MemoryPreparationWorkers, error) {
	if config.Profile != MemoryPreparationCompatibility && config.Profile != MemoryPreparationComplete {
		return nil, domain.ErrValidation
	}
	resources := config.Resources
	if resources == nil || !rootAdapterPresent(resources.meta) || !rootAdapterPresent(resources.blobs) ||
		resources.work == nil || resources.scope == nil || resources.scope.resources == nil || resources.delivery == nil ||
		len(config.Repositories) > domain.MaxMemoryReadWorkerRepos {
		return nil, domain.ErrValidation
	}
	selected := make([]domain.ContentHash, 0, len(config.Repositories))
	seen := make(map[domain.ContentHash]bool, len(config.Repositories))
	for _, repo := range config.Repositories {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		if !seen[repo] {
			seen[repo] = true
			selected = append(selected, repo)
		}
	}
	workers := &MemoryPreparationWorkers{MemoryPreparationResources: resources, memoryReadRepos: selected}
	if len(selected) == 0 {
		return workers, nil
	}
	if err := validateMemoryPreparationPorts(resources.meta, resources.blobs, config.Profile == MemoryPreparationComplete); err != nil {
		return nil, err
	}
	if config.Profile == MemoryPreparationComplete {
		if _, err := workers.memoryLiveQueries(true, false); err != nil {
			return nil, err
		}
	}
	return workers, nil
}

func validateMemoryPreparationPorts(metadata outbound.MetadataStore, blobs outbound.BlobStore, complete bool) error {
	work, _ := metadata.(outbound.MemoryReadWorkStore)
	captures, _ := metadata.(outbound.MemoryCaptureStore)
	anchors, _ := metadata.(outbound.MemoryCaptureAnchorStore)
	reads, _ := metadata.(outbound.MemoryReadStore)
	current, _ := metadata.(outbound.CurrentMemoryReadStore)
	reconciler, _ := metadata.(outbound.MemoryReadWorkReconciler)
	dispatcher, _ := metadata.(outbound.MemoryReadObjectWakeDispatcher)
	minimum, _ := metadata.(outbound.MinimumMemoryReadWorkStore)
	comparison, _ := metadata.(outbound.MemoryReadComparisonStore)
	for _, dependency := range []any{work, captures, anchors, reads, current, reconciler, dispatcher, minimum, comparison} {
		if !rootAdapterPresent(dependency) {
			return domain.ErrCatalogUnsupported
		}
	}
	if !complete {
		return nil
	}
	transactions, _ := metadata.(outbound.RepositoryTransactions)
	snapshots, _ := metadata.(documentQuerySnapshots)
	presence, _ := metadata.(outbound.RepositoryTransactionPresence)
	state, _ := metadata.(outbound.RepositoryTransactionState)
	graphs, _ := metadata.(outbound.RepositoryGraphSourceStore)
	factory, _ := metadata.(outbound.GraphSourceReaderFactory)
	generation, _ := metadata.(outbound.RepositoryMemorySourceGenerationStore)
	graphGeneration, _ := metadata.(memoryquery.LiveGraphGenerationReader)
	pins, _ := metadata.(outbound.MemoryCapturePinReader)
	bounded, _ := blobs.(outbound.BoundedMemoryReader)
	witnesses, _ := blobs.(outbound.WitnessedMemoryReader)
	prPins, _ := metadata.(outbound.PRContributionMemoryReader)
	gitPins, _ := metadata.(outbound.GitContributionMemoryReader)
	evidence, _ := metadata.(outbound.GraphEvidenceReader)
	qualifications, _ := metadata.(outbound.MemoryReadQualificationStore)
	revisions, _ := metadata.(memoryquery.LiveRevisionReader)
	for _, dependency := range []any{transactions, snapshots, presence, state, graphs, factory, generation, graphGeneration, pins, bounded, witnesses, prPins, gitPins, evidence, qualifications, revisions} {
		if !rootAdapterPresent(dependency) {
			return domain.ErrCatalogUnsupported
		}
	}
	return nil
}

func (workers *MemoryPreparationWorkers) Run(ctx context.Context) {
	workers.RunMemoryReadPreparationWorker(ctx)
}
