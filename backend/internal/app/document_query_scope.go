package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentQueryRepoReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type documentQuerySnapshots interface {
	WithinReadSnapshot(context.Context, func(context.Context) error) error
}

type documentQueryScope struct {
	metadata             documentQueryRepoReader
	transactions         documentQuerySnapshots
	presence             outbound.RepositoryTransactionPresence
	supported            []domain.DocumentIdentity
	requireAuthorization bool
}

type documentQueryAuthorizationKey struct{}

type documentQueryAuthorization struct {
	scope         *documentQueryScope
	repo          domain.ContentHash
	permit        *resourcePermit
	actor         string
	system        bool
	authorization *inbound.DocumentReadAuthorizationBinding
}

func (scope *documentQueryScope) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return err
	}
	defer release()
	return scope.WithinDocumentSnapshot(bound, repo, read)
}

func (scope *documentQueryScope) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.supported)
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if permit != nil && !permit.live.Load() {
		return ctx, domain.ErrResourceOverloaded
	}
	actor, system := inbound.RepositoryActor(ctx)
	if current, ok := ctx.Value(documentQueryAuthorizationKey{}).(documentQueryAuthorization); ok && scope.transactions == nil &&
		current.scope == scope && current.repo == repo && current.actor == actor && current.system == system && current.permit == permit && permit != nil && current.authorization == inbound.DocumentReadAuthorizationBindingOf(ctx) {
		return ctx, nil
	}
	if scope.requireAuthorization && !system && inbound.DocumentReadAuthorizer(ctx) == nil && inbound.DocumentSnapshotReadAuthorizer(ctx) == nil {
		return ctx, domain.ErrUnauthorized
	}
	if inbound.DocumentSnapshotReadAuthorizer(ctx) == nil {
		if inbound.DocumentReadAuthorizer(ctx) != nil && scope.presence != nil && scope.presence.InTransaction(ctx) {
			return ctx, domain.ErrCatalogUnsupported
		}
		bound, err := reauthorizeDocumentRead(ctx, repo)
		if err == nil && scope.transactions == nil && permit != nil && permit.live.Load() {
			currentActor, currentSystem := inbound.RepositoryActor(bound)
			bound = context.WithValue(bound, documentQueryAuthorizationKey{}, documentQueryAuthorization{scope: scope, repo: repo, permit: permit, actor: currentActor, system: currentSystem, authorization: inbound.DocumentReadAuthorizationBindingOf(bound)})
		}
		return bound, err
	}
	return ctx, nil
}

func (scope *documentQueryScope) AuthorizeDocumentPreparation(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if scope.presence != nil && scope.presence.InTransaction(ctx) {
		return ctx, domain.ErrConflict
	}
	if outbound.DocReadOnly(ctx) {
		return ctx, domain.ErrForbidden
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return ctx, err
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.supported)
	if inbound.DocumentReadAuthorizer(ctx) != nil {
		return reauthorizeDocumentRead(ctx, repo)
	}
	if _, system := inbound.RepositoryActor(ctx); system {
		return ctx, nil
	}
	return ctx, domain.ErrUnauthorized
}

func (scope *documentQueryScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	return AcquireResources(ctx, ResourceInteractive)
}

func (scope *documentQueryScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return AcquireResources(ctx, ResourceBulk)
}

func (scope *documentQueryScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	ctx, err := scope.AuthorizeDocumentRead(ctx, repo)
	if err != nil {
		return err
	}
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
		var err error
		if authorize := inbound.DocumentSnapshotReadAuthorizer(bound); authorize != nil {
			bound, err = authorize(bound, repo)
		} else if scope.transactions != nil && inbound.DocumentReadAuthorizer(bound) != nil {
			return domain.ErrCatalogUnsupported
		}
		if err != nil {
			return err
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

func (service *Service) DocumentEventQueries() (inbound.DocumentEventQueries, error) {
	return service.DocumentEventQueriesForProfile(documentquery.LegacyCompatibility)
}

func (service *Service) DocumentEventQueriesForProfile(profile documentquery.Profile) (inbound.DocumentEventQueries, error) {
	queries, err := service.newDocumentQueries(profile, false)
	if err != nil {
		return nil, err
	}
	return queries, nil
}

func (service *Service) DocumentQueries() (inbound.DocumentQueries, error) {
	return service.DocumentQueriesForProfile(documentquery.LegacyCompatibility)
}

func (service *Service) DocumentQueriesForProfile(profile documentquery.Profile) (inbound.DocumentQueries, error) {
	queries, err := service.newDocumentQueries(profile, true)
	if err != nil {
		return nil, err
	}
	return queries, nil
}

func (service *Service) newDocumentQueries(profile documentquery.Profile, full bool) (*documentquery.Queries, error) {
	transactions, _ := service.meta.(documentQuerySnapshots)
	presence, _ := service.meta.(outbound.RepositoryTransactionPresence)
	if profile == documentquery.VerifiedRoots && (transactions == nil || presence == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	scope := &documentQueryScope{metadata: service.meta, transactions: transactions,
		presence:             presence,
		supported:            append([]domain.DocumentIdentity(nil), service.DocumentIdentitiesSupported()...),
		requireAuthorization: profile == documentquery.VerifiedRoots}
	indexed, _ := service.blobs.(outbound.DocReadStore)
	verified, _ := service.blobs.(outbound.VerifiedDocReader)
	events, _ := service.blobs.(outbound.VerifiedRootEventReader)
	config := documentquery.Config{Profile: profile,
		Scope: scope, Snapshots: service.meta, Legacy: service.blobs, Index: indexed,
		Verified: verified, RootEvents: events, AtomicRead: transactions != nil}
	if full {
		ranges, _ := service.blobs.(outbound.DocRangeStore)
		rangeSnapshots, _ := service.blobs.(outbound.DocRangeSnapshotStore)
		rangeGenerations, _ := service.blobs.(outbound.DocRangeGenerationStore)
		readiness, _ := service.blobs.(outbound.DocRangeReadinessStore)
		jobs, _ := service.blobs.(outbound.DocRangeJobStore)
		visible, _ := service.blobs.(outbound.DocVisibleGenerationStore)
		visiblePreparation, _ := service.blobs.(outbound.DocVisiblePreparationStore)
		boundaries, _ := service.blobs.(outbound.DocRangeBoundaryStore)
		chunks, _ := service.blobs.(outbound.ConversationChunkReader)
		derived, _ := service.blobs.(outbound.LegacyDerivedByteRangeReader)
		bytes, _ := service.blobs.(outbound.DocByteRangePreparationStore)
		view := &documentquery.ViewDependencies{Range: ranges, RangeSnapshots: rangeSnapshots,
			RangeGenerations: rangeGenerations, RangeReadiness: readiness, RangeJobs: jobs,
			VisibleGenerations: visible, VisiblePreparation: visiblePreparation, Boundaries: boundaries,
			Chunks: chunks, DerivedChunks: derived, Bytes: bytes, Resources: scope}
		if profile == documentquery.LegacyCompatibility {
			view.LegacyPreparation = ranges
		}
		eventDisplay, _ := service.docDisplayRead.(outbound.DocEventDisplayReader)
		config.View = view
		config.Find = &documentquery.FindDependencies{Fields: service.docFindFields,
			DisplayRead: service.docDisplayRead, Display: service.docDisplay, EventDisplay: eventDisplay}
	}
	return documentquery.New(config)
}
