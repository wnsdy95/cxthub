package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryLiveProofScope struct {
	state         outbound.RepositoryTransactionState
	transactional bool
	disabled      bool
}

func (scope *memoryLiveProofScope) CacheableMemoryProof(ctx context.Context, _ domain.ContentHash) bool {
	if scope.disabled {
		return false
	}
	if _, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions); writing {
		return false
	}
	return !scope.transactional || scope.state != nil && scope.state.InReadOnlyTransaction(ctx)
}

type memoryLiveRevisions struct {
	reader memoryquery.LiveRevisionReader
}

func (revisions memoryLiveRevisions) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if revisions.reader == nil {
		return domain.RepositoryRevision{}, nil
	}
	return revisions.reader.RepositoryRevision(ctx, repo)
}

func (service *Service) LiveMemoryQueries() (*memoryquery.LiveQueries, error) {
	return service.memoryLiveQueries(service.requireCompleteMemory, true)
}

func (service *Service) memoryLiveQueries(complete, cache bool) (*memoryquery.LiveQueries, error) {
	if service.projectionMemory != nil {
		return newFrozenMemoryQueries(service.projectionMemory, service.projectionEvidence, service.projectionMemorySource, complete)
	}
	return newLiveMemoryQueries(liveMemoryQueryDependencies{
		metadata: service.meta, objects: service.blobs, projection: service.memoryProjectionReader(),
		reader: service.graphEvidenceReader(), scope: service.memorySnapshotReadScope(), delivery: &service.deliveryCache,
		captured: service.projectionEvidence != nil, complete: complete, cache: cache,
	})
}

type liveMemoryQueryDependencies struct {
	metadata   outbound.MetadataStore
	objects    outbound.BlobStore
	projection outbound.MemoryProjectionReader
	reader     outbound.RepositoryReader
	scope      memoryquery.SnapshotScope
	delivery   *memoryquery.DeliveryCache
	captured   bool
	complete   bool
	cache      bool
}

func newLiveMemoryQueries(dependencies liveMemoryQueryDependencies) (*memoryquery.LiveQueries, error) {
	if !rootAdapterPresent(dependencies.metadata) || !rootAdapterPresent(dependencies.objects) {
		return nil, domain.ErrCatalogUnsupported
	}
	contributions, err := newEvidenceQueries(dependencies.reader, dependencies.captured)
	if err != nil {
		return nil, err
	}
	reader := dependencies.reader
	history, _ := dependencies.metadata.(memoryquery.LiveHistoryReader)
	revisions, _ := dependencies.metadata.(memoryquery.LiveRevisionReader)
	state, _ := dependencies.metadata.(outbound.RepositoryTransactionState)
	_, transactional := dependencies.metadata.(outbound.RepositoryTransactions)
	graphs, _ := dependencies.metadata.(memoryquery.LiveGraphGenerationReader)
	memories, _ := dependencies.metadata.(outbound.RepositoryMemorySourceGenerationStore)
	prPins, _ := dependencies.metadata.(outbound.PRContributionMemoryReader)
	gitPins, _ := dependencies.metadata.(outbound.GitContributionMemoryReader)
	trees, _ := reader.(evidencequery.GitTreeReader)
	deltas, _ := reader.(evidencequery.GitDeltaReader)
	return memoryquery.NewLive(memoryquery.LiveConfig{
		Mode: memoryquery.LiveCurrent, Scope: dependencies.scope, Metadata: dependencies.metadata,
		Projection: dependencies.projection, History: history, Revisions: memoryLiveRevisions{revisions},
		Repository: reader, Evidence: contributions, Trees: trees, Deltas: deltas,
		GraphScope: &legacyGraphProjectionScope{state: state}, GraphGenerations: graphs, MemoryGenerations: memories,
		PRPins: prPins, GitPins: gitPins, Delivery: dependencies.delivery,
		ProofScope: &memoryLiveProofScope{state: state, transactional: transactional, disabled: !dependencies.cache}, RequireComplete: dependencies.complete,
	})
}

func newFrozenMemoryQueries(reader outbound.MemoryQueryReader, evidence outbound.GraphEvidenceReader, generation *domain.RepositoryMemorySourceGeneration, complete bool) (*memoryquery.LiveQueries, error) {
	contributions, err := evidencequery.New(evidencequery.Config{
		Profile: evidencequery.CompleteEvidence, Metadata: evidence, PRLedger: evidence, PRHistory: evidence,
		GitLedger: evidence, GitHeads: evidence, GitAdmission: evidence, GitReceipts: evidence,
	})
	if err != nil {
		return nil, err
	}
	return memoryquery.NewLive(memoryquery.LiveConfig{
		Mode: memoryquery.LiveFrozen, Metadata: reader, Projection: reader, History: reader, Revisions: reader,
		Repository: evidence, Evidence: contributions, Trees: evidence, Deltas: evidence,
		PRPins: reader, GitPins: reader, FrozenGeneration: generation, CapturedEvidence: true, RequireComplete: complete,
	})
}
