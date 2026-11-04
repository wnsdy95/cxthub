package app

import (
	"context"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type preparedMemoryQueryScope struct {
	ownerKey      *memoryReadOwner
	forceGeneral  bool
	transactional bool
	snapshots     bool
	presence      outbound.RepositoryTransactionPresence
}

type preparedMemoryScopeKey struct{}

type memoryReadOwner struct {
	marker uint8
}

type preparedMemoryScope struct {
	owner         *preparedMemoryQueryScope
	ownerKey      *memoryReadOwner
	transactional bool
	snapshots     bool
	repo          domain.ContentHash
	permit        *resourcePermit
	actor         string
	system        bool
	authorization *inbound.DocumentReadAuthorizationBinding
	general       bool
	active        atomic.Bool
}

func (scope *preparedMemoryQueryScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if domain.ValidateContentHash(repo) != nil || read == nil {
		return domain.ErrValidation
	}
	_, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	borrowed := scope.transactional && (scope.presence == nil || scope.presence.InTransaction(ctx))
	authorize := inbound.DocumentReadAuthorizer(ctx)
	_, system := inbound.RepositoryActor(ctx)
	bound, release, err := acquireResources(ctx, ResourceBulk, !writing && !borrowed && (authorize != nil || system))
	if err != nil {
		return err
	}
	defer release()
	permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
	actor, system := inbound.RepositoryActor(bound)
	if current, ok := bound.Value(preparedMemoryScopeKey{}).(*preparedMemoryScope); ok &&
		((scope.ownerKey != nil && current.ownerKey == scope.ownerKey) || (scope.ownerKey == nil && current.owner == scope)) &&
		current.transactional == scope.transactional && current.snapshots == scope.snapshots &&
		(!scope.forceGeneral || authorize == nil || current.general) &&
		current.repo == repo && current.permit == permit && permit != nil && permit.live.Load() && current.active.Load() &&
		current.actor == actor && current.system == system && current.authorization == inbound.DocumentReadAuthorizationBindingOf(bound) {
		return read(bound)
	}
	general := false
	if authorize != nil && (scope.forceGeneral || !scope.snapshots || inbound.DocumentSnapshotReadAuthorizer(bound) == nil) {
		if borrowed || scope.presence != nil && scope.presence.InTransaction(bound) {
			return domain.ErrCatalogUnsupported
		}
		bound, err = authorize(bound, repo)
		if err != nil {
			return err
		}
		general = true
	}
	actor, system = inbound.RepositoryActor(bound)
	current := &preparedMemoryScope{owner: scope, ownerKey: scope.ownerKey, transactional: scope.transactional, snapshots: scope.snapshots,
		repo: repo, permit: permit, actor: actor, system: system,
		authorization: inbound.DocumentReadAuthorizationBindingOf(bound), general: general}
	current.active.Store(true)
	defer current.active.Store(false)
	return read(context.WithValue(bound, preparedMemoryScopeKey{}, current))
}

func (*preparedMemoryQueryScope) AuthorizeSnapshot(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return authorizeMemoryReadSnapshot(ctx, repo)
}

func (scope *preparedMemoryQueryScope) authorizePreparation(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if outbound.DocReadOnly(ctx) || scope.transactional && scope.presence == nil || scope.presence != nil && scope.presence.InTransaction(ctx) {
		return ctx, domain.ErrCatalogUnsupported
	}
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	if _, system := inbound.RepositoryActor(ctx); system {
		return ctx, nil
	}
	return ctx, domain.ErrUnauthorized
}

type preparedMemoryRequester struct {
	scope     *preparedMemoryQueryScope
	minimum   *memoryquery.MinimumQueries
	storage   outbound.MemoryReadDemandStore
	supported []domain.DocumentIdentity
}

func (requester *preparedMemoryRequester) RequestPrepared(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement) error {
	bound, err := requester.scope.authorizePreparation(ctx, target.RepoID)
	if err != nil {
		return err
	}
	bound = outbound.WithDocumentIdentityCompatibility(bound, inbound.DocumentIdentities(bound), requester.supported)
	request := domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content}
	current, err := requester.minimum.ObserveMemoryReadRequirement(bound, target.RepoID, request)
	if err != nil {
		return err
	}
	if err := domain.CheckMemorySourceMinimum(current.Minimum, requirement.Minimum); err != nil {
		if domain.IsOnlyMemoryReadPending(err) {
			return domain.ErrValidation
		}
		return err
	}
	work, err := requester.storage.RequestMinimumMemoryReadWork(bound, target, requirement)
	if err != nil {
		return err
	}
	if work.State == "rejected" {
		return domain.ErrMemoryReadNeedsAttention
	}
	return nil
}

func (service *Service) minimumMemoryQueries() (*memoryquery.MinimumQueries, *preparedMemoryQueryScope, error) {
	scope := service.memoryResourceScope()
	if scope.snapshots && scope.presence == nil {
		return nil, nil, domain.ErrCatalogUnsupported
	}
	reader, supported := service.meta.(outbound.MinimumMemoryReadStore)
	profile := memoryquery.Unavailable
	if supported {
		profile = memoryquery.Available
	}
	queries, err := memoryquery.NewMinimum(memoryquery.MinimumConfig{Profile: profile, Scope: scope,
		Reader: reader, SupportedIdentities: service.DocumentIdentitiesSupported()})
	return queries, scope, err
}

func (service *Service) memoryResourceScope() *preparedMemoryQueryScope {
	owner := service.memoryReadOwner.Load()
	if owner == nil {
		created := &memoryReadOwner{}
		if service.memoryReadOwner.CompareAndSwap(nil, created) {
			owner = created
		} else {
			owner = service.memoryReadOwner.Load()
		}
	}
	return newMemoryResourceScope(service.meta, owner)
}

func (service *Service) PreparedMemoryQueries() (*memoryquery.PreparedQueries, error) {
	minimum, scope, err := service.minimumMemoryQueries()
	if err != nil {
		return nil, err
	}
	config := memoryquery.PreparedConfig{Scope: scope, Minimum: minimum}
	for repo, enabled := range service.preparedMemoryRepos {
		if enabled {
			config.Repositories = append(config.Repositories, repo)
		}
	}
	for repo, enabled := range service.preparedMemoryDemands {
		if enabled {
			config.DemandRepositories = append(config.DemandRepositories, repo)
		}
	}
	if len(config.DemandRepositories) != 0 {
		storage, supported := service.meta.(outbound.MemoryReadDemandStore)
		if !supported {
			return nil, domain.ErrCatalogUnsupported
		}
		config.Requester = &preparedMemoryRequester{scope: scope, minimum: minimum, storage: storage,
			supported: service.DocumentIdentitiesSupported()}
	}
	return memoryquery.NewPrepared(config)
}
