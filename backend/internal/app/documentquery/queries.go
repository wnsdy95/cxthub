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
	AuthorizeDocumentPreparation(context.Context, domain.ContentHash) (context.Context, error)
	WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error
}

type SnapshotReader interface {
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
}

type LegacyReader interface {
	GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error)
	GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error)
	GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
}

type Config struct {
	Profile    Profile
	Scope      Scope
	Snapshots  SnapshotReader
	Legacy     LegacyReader
	Index      outbound.DocReadStore
	Verified   outbound.VerifiedDocReader
	RootEvents outbound.VerifiedRootEventReader
	AtomicRead bool
	ReadOnly   bool
	View       *ViewDependencies
	Find       *FindDependencies
}

type Queries struct {
	scope      Scope
	metadata   SnapshotReader
	legacy     LegacyReader
	index      outbound.DocReadStore
	verified   outbound.VerifiedDocReader
	rootEvents outbound.VerifiedRootEventReader
	atomicRead bool
	readOnly   bool
	view       ViewDependencies
	find       FindDependencies
}

var _ inbound.DocumentQueries = (*Queries)(nil)

func New(config Config) (*Queries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != VerifiedRoots {
		return nil, fmt.Errorf("%w: document query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Snapshots, config.Legacy} {
		if absent(required) {
			return nil, fmt.Errorf("%w: missing document query dependency", domain.ErrValidation)
		}
	}
	for _, optional := range []any{config.Index, config.Verified, config.RootEvents} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: typed-nil document query dependency", domain.ErrValidation)
		}
	}
	if config.Profile == VerifiedRoots && (config.Index == nil || config.Verified == nil || config.RootEvents == nil) {
		return nil, fmt.Errorf("%w: verified document query dependencies are required", domain.ErrUnsupportedDocumentIdentity)
	}
	queries := &Queries{scope: config.Scope, metadata: config.Snapshots, legacy: config.Legacy, index: config.Index,
		verified: config.Verified, rootEvents: config.RootEvents, atomicRead: config.AtomicRead, readOnly: config.ReadOnly}
	if config.View != nil {
		if err := config.View.validate(config.Profile, config.ReadOnly); err != nil {
			return nil, err
		}
		queries.view = *config.View
	}
	if config.Find != nil {
		if config.View == nil {
			return nil, fmt.Errorf("%w: document find requires view dependencies", domain.ErrValidation)
		}
		if err := config.Find.validate(config.ReadOnly); err != nil {
			return nil, err
		}
		queries.find = *config.Find
	}
	return queries, nil
}

func (queries *Queries) readContext(ctx context.Context) context.Context {
	if queries.readOnly {
		return outbound.WithDocReadOnly(ctx)
	}
	return ctx
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
