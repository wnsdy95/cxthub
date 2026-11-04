package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type publishedGraphQueryScope struct {
	repository    *repositoryQueryScope
	transactional bool
	presence      outbound.RepositoryTransactionPresence
}

func (scope *publishedGraphQueryScope) borrowed(ctx context.Context) bool {
	if _, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions); writing {
		return true
	}
	return scope.transactional && (scope.presence == nil || scope.presence.InTransaction(ctx))
}

func (scope *publishedGraphQueryScope) ReadGraph(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.readGraphWithRequirement(ctx, repo, true, read)
}

func (scope *publishedGraphQueryScope) readGraphWithRequirement(ctx context.Context, repo domain.ContentHash, required bool, read func(context.Context) error) error {
	work := func(bound context.Context) error {
		bound, err := scope.repository.AuthorizeRead(bound, repo, required)
		if err != nil {
			return err
		}
		return scope.repository.WithinRepositoryRead(bound, repo, read)
	}
	if scope.borrowed(ctx) {
		err := resourceWorkWithWait(ctx, ResourceInteractive, false, work)
		if errors.Is(err, errGraphReadNeedsBulk) {
			return domain.ErrResourceOverloaded
		}
		return err
	}
	return WithGraphReadResources(ctx, work)
}

func (scope *publishedGraphQueryScope) GraphRepairAllowed(ctx context.Context) bool {
	return !scope.borrowed(ctx) && !outbound.DocReadOnly(ctx)
}

func (scope *publishedGraphQueryScope) RepairGraph(ctx context.Context, repo domain.ContentHash, repair func(context.Context) error) error {
	if !scope.GraphRepairAllowed(ctx) {
		return domain.ErrConflict
	}
	finish, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	finish = outbound.WithDocumentIdentityCompatibility(finish, inbound.DocumentIdentities(ctx), scope.repository.supported)
	return resourceWork(finish, ResourceInteractive, func(bound context.Context) error {
		bound, err := scope.repository.AuthorizeRead(bound, repo, true)
		if err != nil {
			return err
		}
		return repair(bound)
	})
}

func (*publishedGraphQueryScope) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	return acquireGraphBuild(ctx)
}

func (service *Service) PublishedGraphQueries() (inbound.RepositoryGraphQueries, error) {
	transactions, _ := service.meta.(documentQuerySnapshots)
	presence, _ := service.meta.(outbound.RepositoryTransactionPresence)
	_, transactional := service.meta.(outbound.RepositoryTransactions)
	scope := &publishedGraphQueryScope{repository: &repositoryQueryScope{metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported()},
		transactional: transactional, presence: presence}
	reader, _ := service.meta.(repositoryquery.PublishedGraphReader)
	serving, _ := service.meta.(repositoryquery.ServingGraphRepair)
	projection, _ := service.meta.(repositoryquery.ProjectionGraphRepair)
	profile := repositoryquery.LegacyCompatibility
	if transactions != nil {
		profile = repositoryquery.PinnedSnapshots
	}
	config := repositoryquery.PublishedGraphConfig{Profile: profile, Scope: scope, Resources: scope, Reader: reader,
		ServingRepairs: serving, ProjectionRepairs: projection}
	for repo, enabled := range service.graphReadRepos {
		if enabled {
			config.Repos = append(config.Repos, repo)
		}
	}
	return repositoryquery.NewPublishedGraph(config)
}
