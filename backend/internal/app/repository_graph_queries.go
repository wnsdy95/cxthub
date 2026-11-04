package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyGraphQueryScope struct {
	*publishedGraphQueryScope
}

func (scope *legacyGraphQueryScope) ReadGraph(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.readGraphWithRequirement(ctx, repo, false, read)
}

func (service *Service) legacyGraphQueriesForProfile(profile repositoryquery.Profile) (*repositoryquery.LegacyGraphQueries, error) {
	transactions, _ := service.meta.(documentQuerySnapshots)
	if profile == repositoryquery.PinnedSnapshots && transactions == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	presence, _ := service.meta.(outbound.RepositoryTransactionPresence)
	_, transactional := service.meta.(outbound.RepositoryTransactions)
	scope := &legacyGraphQueryScope{publishedGraphQueryScope: &publishedGraphQueryScope{
		repository:    &repositoryQueryScope{metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported()},
		transactional: transactional, presence: presence,
	}}
	projection, err := service.legacyGraphProjection()
	if err != nil {
		return nil, err
	}
	history, _ := service.meta.(repositoryquery.HistoryReader)
	revisions, _ := service.meta.(repositoryquery.RevisionReader)
	archives, _ := service.meta.(repositoryquery.ArchiveReader)
	return repositoryquery.NewLegacyGraph(repositoryquery.LegacyGraphConfig{
		Profile: profile, Scope: scope, Metadata: service.meta, History: history, Revisions: revisions, Archives: archives, Projection: projection,
	})
}

func (service *Service) GraphQueries() (inbound.RepositoryGraphQueries, error) {
	profile := inbound.RepositoryGraphCompatibility
	if _, pinned := service.meta.(documentQuerySnapshots); pinned {
		profile = inbound.RepositoryGraphPinnedSnapshots
	}
	return service.GraphQueriesForProfile(profile)
}

func (service *Service) GraphQueriesForProfile(profile inbound.RepositoryGraphQueryProfile) (inbound.RepositoryGraphQueries, error) {
	if profile != inbound.RepositoryGraphCompatibility && profile != inbound.RepositoryGraphPinnedSnapshots {
		return nil, domain.ErrValidation
	}
	legacy, err := service.legacyGraphQueriesForProfile(profile)
	if err != nil {
		return nil, err
	}
	config := repositoryquery.GraphConfig{Legacy: legacy}
	for repo, enabled := range service.graphReadRepos {
		if enabled {
			config.PublishedRepos = append(config.PublishedRepos, repo)
		}
	}
	if len(config.PublishedRepos) != 0 {
		config.Published, err = service.PublishedGraphQueries()
		if err != nil {
			return nil, err
		}
	}
	return repositoryquery.NewGraph(config)
}
