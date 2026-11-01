package memoryquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ServingScope interface {
	SnapshotScope
	AuthorizeSnapshot(context.Context, domain.ContentHash) (context.Context, error)
}

type WorkStatusReader interface {
	ReadMemoryReadWork(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryReadWork, error)
}

type ServingConfig struct {
	Scope               ServingScope
	Current             outbound.CurrentMemoryReadStore
	Qualified           outbound.MemoryReadQualificationReader
	WorkStatus          WorkStatusReader
	Transactions        outbound.RepositoryTransactionPresence
	SupportedIdentities []domain.DocumentIdentity
}

type ServingQueries struct {
	scope        ServingScope
	current      outbound.CurrentMemoryReadStore
	qualified    outbound.MemoryReadQualificationReader
	workStatus   WorkStatusReader
	transactions outbound.RepositoryTransactionPresence
	supported    []domain.DocumentIdentity
}

var _ inbound.MemoryReadScope = (*ServingQueries)(nil)
var _ outbound.MemoryReadRangeStore = currentMemoryRangeReader{}
var _ outbound.MemoryReadRangeStore = qualifiedMemoryRangeReader{}

func NewServing(config ServingConfig) (*ServingQueries, error) {
	if absent(config.Scope) {
		return nil, domain.ErrCatalogUnsupported
	}
	for _, dependency := range []any{config.Current, config.Qualified, config.WorkStatus, config.Transactions} {
		if dependency != nil && absent(dependency) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	for _, identity := range config.SupportedIdentities {
		if err := identity.Validate(); err != nil {
			return nil, err
		}
	}
	return &ServingQueries{scope: config.Scope, current: config.Current, qualified: config.Qualified,
		workStatus: config.WorkStatus, transactions: config.Transactions,
		supported: append([]domain.DocumentIdentity(nil), config.SupportedIdentities...)}, nil
}

func (queries *ServingQueries) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if queries == nil || absent(queries.scope) {
		return domain.ErrCatalogUnsupported
	}
	return queries.scope.WithMemoryReadResources(ctx, repo, read)
}

func AuthorizeSnapshot(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentSnapshotReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	if _, system := inbound.RepositoryActor(ctx); system && inbound.DocumentReadAuthorizer(ctx) == nil {
		return ctx, nil
	}
	return ctx, domain.ErrCatalogUnsupported
}

type currentMemoryRangeReader struct {
	scope   ServingScope
	storage outbound.CurrentMemoryReadStore
	target  domain.MemoryReadTarget
}

func (reader currentMemoryRangeReader) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	expected, err := domain.MemoryReadTargetID(ctx, reader.target)
	if err != nil {
		return err
	}
	if repo != reader.target.RepoID || identity != expected {
		return domain.ErrValidation
	}
	return reader.storage.WithCurrentMemoryReadRange(ctx, reader.target, func(current context.Context) (context.Context, error) {
		return reader.scope.AuthorizeSnapshot(current, repo)
	}, consume)
}

type qualifiedMemoryRangeReader struct {
	scope   ServingScope
	storage outbound.MemoryReadQualificationReader
}

func (reader qualifiedMemoryRangeReader) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	return reader.storage.WithQualifiedMemoryReadRange(ctx, repo, identity, func(current context.Context) (context.Context, error) {
		return reader.scope.AuthorizeSnapshot(current, repo)
	}, consume)
}

func (queries *ServingQueries) QueryCurrentMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	target, err := ReadTarget(repo, request)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	if queries == nil || queries.current == nil {
		return domain.EffectiveMemoryPage{}, domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), queries.supported)
	var result domain.EffectiveMemoryPage
	err = queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		result, err = ReadPage(bound, currentMemoryRangeReader{scope: queries.scope, storage: queries.current, target: target}, repo, identifier, request)
		return err
	})
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return result, nil
}

func (queries *ServingQueries) QueryQualifiedMemory(ctx context.Context, repo, qualification domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if queries == nil || queries.qualified == nil {
		return domain.EffectiveMemoryPage{}, domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), queries.supported)
	var result domain.EffectiveMemoryPage
	err := queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		result, err = ReadPage(bound, qualifiedMemoryRangeReader{scope: queries.scope, storage: queries.qualified}, repo, qualification, request)
		return err
	})
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return result, nil
}

func (queries *ServingQueries) ReadMemoryPreparationStatus(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadWork, error) {
	if _, system := inbound.RepositoryActor(ctx); !system || inbound.DocumentReadAuthorizer(ctx) != nil {
		return domain.MemoryReadWork{}, domain.ErrForbidden
	}
	if queries == nil || queries.transactions == nil {
		return domain.MemoryReadWork{}, domain.ErrCatalogUnsupported
	}
	if queries.transactions.InTransaction(ctx) {
		return domain.MemoryReadWork{}, domain.ErrConflict
	}
	target, err := ReadTarget(repo, request)
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	if queries.workStatus == nil {
		return domain.MemoryReadWork{}, domain.ErrCatalogUnsupported
	}
	var result domain.MemoryReadWork
	err = queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		return queries.scope.WithinMemorySnapshot(bound, repo, func(current context.Context) error {
			var err error
			result, err = queries.workStatus.ReadMemoryReadWork(current, repo, identifier)
			return err
		})
	})
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	return result, nil
}
