package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryQueryScope struct {
	metadata     documentQueryRepoReader
	transactions documentQuerySnapshots
	supported    []domain.DocumentIdentity
}

func (*repositoryQueryScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	return AcquireResources(ctx, ResourceInteractive)
}

func (scope *repositoryQueryScope) AuthorizeRead(ctx context.Context, repo domain.ContentHash, required bool) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		bound, err := authorize(ctx, repo)
		if err == nil && scope.transactions != nil && inbound.DocumentSnapshotReadAuthorizer(bound) == nil {
			return bound, domain.ErrCatalogUnsupported
		}
		return bound, err
	}
	if _, system := inbound.RepositoryActor(ctx); required && !system {
		return ctx, domain.ErrForbidden
	}
	return ctx, nil
}

func (scope *repositoryQueryScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.supported)
	execute := func(bound context.Context) error {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		if err := bound.Err(); err != nil {
			return err
		}
		for _, identity := range inbound.DocumentIdentities(bound) {
			if err := identity.Validate(); err != nil {
				return err
			}
		}
		if authorize := inbound.DocumentSnapshotReadAuthorizer(bound); scope.transactions != nil && authorize != nil {
			var err error
			bound, err = authorize(bound, repo)
			if err != nil {
				return err
			}
		}
		metadata, err := scope.metadata.GetRepo(bound, repo)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil {
			if err := metadata.RequiredDocIdentity.Validate(); err != nil {
				return err
			}
			if !hasDocumentIdentity(scope.supported, metadata.RequiredDocIdentity) || !hasDocumentIdentity(inbound.DocumentIdentities(bound), metadata.RequiredDocIdentity) {
				return domain.ErrDocumentIdentityUpgradeRequired
			}
		}
		return read(bound)
	}
	if scope.transactions != nil {
		return scope.transactions.WithinReadSnapshot(ctx, execute)
	}
	return execute(ctx)
}

func (service *Service) RepositoryStateQueries() (inbound.RepositoryStateQueries, error) {
	profile := repositoryquery.LegacyCompatibility
	if _, supported := service.meta.(documentQuerySnapshots); supported {
		profile = repositoryquery.PinnedSnapshots
	}
	return service.RepositoryStateQueriesForProfile(profile)
}

func (service *Service) RepositoryStateQueriesForProfile(profile repositoryquery.Profile) (inbound.RepositoryStateQueries, error) {
	transactions, _ := service.meta.(documentQuerySnapshots)
	if profile == repositoryquery.PinnedSnapshots && transactions == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	scope := &repositoryQueryScope{metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported()}
	revisions, _ := service.meta.(repositoryquery.RevisionReader)
	history, _ := service.meta.(repositoryquery.HistoryReader)
	archives, _ := service.meta.(repositoryquery.ArchiveReader)
	pending, _ := service.meta.(repositoryquery.PendingReader)
	pendingState, _ := service.meta.(repositoryquery.PendingStatusReader)
	graphState, _ := service.meta.(repositoryquery.GraphStatusReader)
	config := repositoryquery.Config{Profile: profile, Scope: scope, Resources: scope, Metadata: service.meta,
		Revisions: revisions, History: history, Archives: archives, Pending: pending, PendingState: pendingState, GraphState: graphState,
		AtomicRead: transactions != nil}
	for repo, enabled := range service.pendingProjectionRepos {
		if enabled {
			config.PendingRepos = append(config.PendingRepos, repo)
		}
	}
	for repo, enabled := range service.graphReadRepos {
		if enabled {
			config.GraphRepos = append(config.GraphRepos, repo)
		}
	}
	return repositoryquery.New(config)
}
