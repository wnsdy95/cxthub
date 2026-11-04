package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func newMemoryResourceScope(metadata outbound.MetadataStore, owner *memoryReadOwner) *preparedMemoryQueryScope {
	_, transactional := metadata.(outbound.RepositoryTransactions)
	_, snapshots := metadata.(documentQuerySnapshots)
	presence, _ := metadata.(outbound.RepositoryTransactionPresence)
	return &preparedMemoryQueryScope{ownerKey: owner, transactional: transactional, snapshots: snapshots, presence: presence}
}

func (workers *MemoryPreparationWorkers) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope := *workers.scope.resources
	scope.forceGeneral = true
	return scope.WithMemoryReadResources(ctx, repo, read)
}

func memoryPreparationReadForRepo[Result any](ctx context.Context, workers *MemoryPreparationWorkers, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := workers.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		result, err = repositoryReadForRepoWithWork(bound, workers.work, repo, read)
		return err
	})
	if err != nil {
		var zero Result
		return zero, err
	}
	return result, nil
}

func (workers *MemoryPreparationWorkers) DocumentIdentitiesSupported() []domain.DocumentIdentity {
	return append([]domain.DocumentIdentity(nil), workers.work.supported...)
}

func (workers *MemoryPreparationWorkers) DocumentIdentityWorkerContext(ctx context.Context) context.Context {
	return inbound.WithDocumentIdentities(ctx, workers.DocumentIdentitiesSupported())
}

func (workers *MemoryPreparationWorkers) graphEvidenceReader() outbound.RepositoryReader {
	if workers.projectionEvidence != nil {
		return workers.projectionEvidence
	}
	return workers.meta
}

func (workers *MemoryPreparationWorkers) memorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, *domain.RepositoryMemorySourceGeneration, error) {
	return readMemorySourceGeneration(ctx, workers.meta, repo, workers.projectionMemory, workers.projectionEvidence, workers.projectionMemorySource)
}

func (workers *MemoryPreparationWorkers) memoryLiveQueries(complete, cache bool) (*memoryquery.LiveQueries, error) {
	if workers.projectionMemory != nil {
		return newFrozenMemoryQueries(workers.projectionMemory, workers.projectionEvidence, workers.projectionMemorySource, complete)
	}
	return newLiveMemoryQueries(liveMemoryQueryDependencies{
		metadata: workers.meta, objects: workers.blobs, projection: serviceProjectionSource{workers.meta, workers.blobs},
		reader: workers.graphEvidenceReader(), scope: workers.scope, delivery: workers.delivery,
		captured: workers.projectionEvidence != nil, complete: complete, cache: cache,
	})
}
