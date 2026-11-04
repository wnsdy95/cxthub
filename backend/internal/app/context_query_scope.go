package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type contextQueryScope struct {
	repository *repositoryQueryScope
	presence   outbound.RepositoryTransactionPresence
}

func (scope *contextQueryScope) ReadContext(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		return err
	}
	defer release()
	if scope.repository.transactions == nil || inbound.DocumentSnapshotReadAuthorizer(bound) == nil {
		if inbound.DocumentReadAuthorizer(bound) != nil && scope.presence != nil && scope.presence.InTransaction(bound) {
			return domain.ErrCatalogUnsupported
		}
		bound, err = scope.repository.AuthorizeRead(bound, repo, false)
		if err != nil {
			return err
		}
	}
	return scope.repository.WithinRepositoryRead(bound, repo, read)
}

func (service *Service) ContextQueries() (inbound.ContextQuery, error) {
	profile := inbound.ContextQueryCompatibility
	if _, pinned := service.meta.(documentQuerySnapshots); pinned {
		profile = inbound.ContextQueryPinnedSnapshots
	}
	return service.ContextQueriesForProfile(profile)
}

func (service *Service) ContextQueriesForProfile(profile inbound.ContextQueryProfile) (inbound.ContextQuery, error) {
	if profile != inbound.ContextQueryCompatibility && profile != inbound.ContextQueryPinnedSnapshots {
		return nil, domain.ErrValidation
	}
	transactions, _ := service.meta.(documentQuerySnapshots)
	if profile == inbound.ContextQueryPinnedSnapshots && transactions == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	indexes, err := service.newDocumentQueries(documentquery.LegacyCompatibility, false)
	if err != nil {
		return nil, err
	}
	contributions, err := service.evidenceQueries()
	if err != nil {
		return nil, err
	}
	reader := service.graphEvidenceReader()
	trees, _ := reader.(evidencequery.GitTreeReader)
	deltas, _ := reader.(evidencequery.GitDeltaReader)
	state, _ := service.meta.(outbound.RepositoryTransactionState)
	generations, _ := service.meta.(repositoryquery.GraphGenerationReader)
	history, _ := service.meta.(repositoryquery.HistoryReader)
	revisions, _ := service.meta.(repositoryquery.RevisionReader)
	archives, _ := service.meta.(repositoryquery.ArchiveReader)
	presence, _ := service.meta.(outbound.RepositoryTransactionPresence)
	scope := &contextQueryScope{repository: &repositoryQueryScope{
		metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported(),
	}, presence: presence}
	return repositoryquery.NewContext(repositoryquery.ContextConfig{
		Profile: profile, Scope: scope, Metadata: service.meta, History: history, Revisions: revisions, Archives: archives,
		Indexes: indexes, CacheScope: &legacyGraphProjectionScope{state: state}, Generations: generations,
		Cache: &service.segmentCache, Contributions: contributions, EvidenceMetadata: reader, Trees: trees, Deltas: deltas,
		Captured: service.projectionEvidence != nil, RequireComplete: service.requireCompleteMemory, AtomicRead: transactions != nil,
	})
}
