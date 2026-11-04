package memoryquery

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Scope interface {
	inbound.MemoryReadScope
	AuthorizeSnapshot(context.Context, domain.ContentHash) (context.Context, error)
}

type Profile uint8

const (
	Unavailable Profile = iota + 1
	Available
)

type MinimumConfig struct {
	Profile             Profile
	Scope               Scope
	Reader              outbound.MinimumMemoryReadStore
	SupportedIdentities []domain.DocumentIdentity
}

type MinimumQueries struct {
	scope     Scope
	reader    outbound.MinimumMemoryReadStore
	supported []domain.DocumentIdentity
}

func NewMinimum(config MinimumConfig) (*MinimumQueries, error) {
	if config.Profile != Unavailable && config.Profile != Available {
		return nil, fmt.Errorf("%w: minimum memory profile is required", domain.ErrValidation)
	}
	if absent(config.Scope) || config.Reader != nil && absent(config.Reader) {
		return nil, fmt.Errorf("%w: minimum memory query dependency is required", domain.ErrCatalogUnsupported)
	}
	if config.Profile == Available && config.Reader == nil || config.Profile == Unavailable && config.Reader != nil {
		return nil, domain.ErrCatalogUnsupported
	}
	for _, identity := range config.SupportedIdentities {
		if err := identity.Validate(); err != nil {
			return nil, err
		}
	}
	return &MinimumQueries{scope: config.Scope, reader: config.Reader, supported: append([]domain.DocumentIdentity(nil), config.SupportedIdentities...)}, nil
}

func (queries *MinimumQueries) Available() bool {
	return queries != nil && queries.reader != nil
}

type PreparationRequester interface {
	RequestPrepared(context.Context, domain.MemoryReadTarget, domain.MemoryReadRequirement) error
}

type PreparedConfig struct {
	Scope              Scope
	Minimum            *MinimumQueries
	Repositories       []domain.ContentHash
	DemandRepositories []domain.ContentHash
	Requester          PreparationRequester
}

type PreparedQueries struct {
	scope      Scope
	minimum    *MinimumQueries
	configured map[domain.ContentHash]bool
	demands    map[domain.ContentHash]bool
	requester  PreparationRequester
}

func NewPrepared(config PreparedConfig) (*PreparedQueries, error) {
	if absent(config.Scope) || config.Requester != nil && absent(config.Requester) {
		return nil, domain.ErrCatalogUnsupported
	}
	if len(config.Repositories) > domain.MaxMemoryReadWorkerRepos || len(config.DemandRepositories) > domain.MaxMemoryReadWorkerRepos {
		return nil, domain.ErrValidation
	}
	if len(config.Repositories) != 0 && !config.Minimum.Available() || len(config.DemandRepositories) != 0 && config.Requester == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	queries := &PreparedQueries{scope: config.Scope, minimum: config.Minimum, configured: map[domain.ContentHash]bool{}, demands: map[domain.ContentHash]bool{}, requester: config.Requester}
	for _, repo := range config.Repositories {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		queries.configured[repo] = true
	}
	for _, repo := range config.DemandRepositories {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		queries.demands[repo] = true
	}
	return queries, nil
}

func (queries *PreparedQueries) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return queries.scope.WithMemoryReadResources(ctx, repo, read)
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	}
	return false
}
