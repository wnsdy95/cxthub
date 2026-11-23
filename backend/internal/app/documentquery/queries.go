package documentquery

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Profile = inbound.DocumentQueryProfile

const (
	LegacyCompatibility = inbound.DocumentQueryCompatibility
	VerifiedRoots       = inbound.DocumentQueryVerifiedRoots
)

type Scope interface {
	AuthorizeDocumentRead(context.Context, domain.ContentHash) (context.Context, error)
	WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error
}

// LegacyReader reads a stored conversation document, whole or by its chunks.
type LegacyReader interface {
	GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error)
	GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error)
	GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
}

// Resources admits a read before it does any work.
type Resources interface {
	AcquireInteractive(context.Context) (context.Context, func(), error)
	AcquireBulk(context.Context) (context.Context, func(), error)
}

// Config names what the reader works with. Index is the stored read index of
// documents; without it the index is built from the whole document. Resources
// is needed by the paged reads only.
type Config struct {
	Profile    Profile
	Scope      Scope
	Legacy     LegacyReader
	Index      outbound.DocReadStore
	Resources  Resources
	AtomicRead bool
	ReadOnly   bool
}

type Queries struct {
	scope      Scope
	legacy     LegacyReader
	index      outbound.DocReadStore
	resources  Resources
	atomicRead bool
	readOnly   bool
}

var _ inbound.DocumentQueries = (*Queries)(nil)

func New(config Config) (*Queries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != VerifiedRoots {
		return nil, fmt.Errorf("%w: document query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Legacy} {
		if absent(required) {
			return nil, fmt.Errorf("%w: missing document query dependency", domain.ErrValidation)
		}
	}
	for _, optional := range []any{config.Index, config.Resources} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: typed-nil document query dependency", domain.ErrValidation)
		}
	}
	if config.Profile == VerifiedRoots && config.Index == nil {
		return nil, fmt.Errorf("%w: the stored read index is required", domain.ErrUnsupportedDocumentIdentity)
	}
	return &Queries{scope: config.Scope, legacy: config.Legacy, index: config.Index, resources: config.Resources,
		atomicRead: config.AtomicRead, readOnly: config.ReadOnly}, nil
}

func (queries *Queries) readContext(ctx context.Context) context.Context {
	if queries.readOnly {
		return outbound.WithDocReadOnly(ctx)
	}
	return ctx
}

func (queries *Queries) reauthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return queries.scope.AuthorizeDocumentRead(queries.readContext(ctx), repo)
}

func (queries *Queries) acquireViewInteractive(ctx context.Context) (context.Context, func(), error) {
	ctx = queries.readContext(ctx)
	if queries.resources == nil {
		return ctx, nil, domain.ErrDocRangeIndexUnavailable
	}
	return queries.resources.AcquireInteractive(ctx)
}

func (queries *Queries) acquireViewBulk(ctx context.Context) (context.Context, func(), error) {
	ctx = queries.readContext(ctx)
	if queries.resources == nil {
		return ctx, nil, domain.ErrDocRangeIndexUnavailable
	}
	return queries.resources.AcquireBulk(ctx)
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

func readDocument[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error), snapshot bool) (Result, error) {
	ctx = queries.readContext(ctx)
	var result Result
	execute := queries.scope.WithinDocumentRead
	if snapshot {
		execute = queries.scope.WithinDocumentSnapshot
	}
	err := execute(ctx, repo, func(bound context.Context) error {
		bound = queries.readContext(bound)
		var err error
		result, err = read(bound)
		return err
	})
	if err != nil && queries.atomicRead {
		var empty Result
		return empty, err
	}
	return result, err
}
