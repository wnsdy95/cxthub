package projectionworker

import (
	"context"
	"reflect"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Profile string

const (
	Compatibility Profile = "compatibility"
	Complete      Profile = "complete"
)

type WorkScope interface {
	WorkerContext(context.Context) context.Context
	WithinWork(context.Context, func(context.Context) error) error
	CheckRepository(context.Context, domain.ContentHash) error
}

type GraphScope interface {
	WorkScope
	repositoryquery.GraphResources
	WithinRepositoryRead(context.Context, domain.ContentHash, func(context.Context) error) error
}

type QualificationScope interface {
	WorkScope
	WithinRead(context.Context, func(context.Context) error) error
	ValidateQualificationStart(context.Context) error
	InReadOnlyTransaction(context.Context) bool
	CheckIdentity(context.Context, domain.ContentHash) error
}

type ReferenceReader interface {
	Reference(context.Context, domain.ContentHash) (domain.ContentHash, error)
}

type PendingConfig struct {
	Profile Profile
	Scope   WorkScope
	Jobs    outbound.PendingProjectionStore
	Source  outbound.RepositoryViewSourceStore
	Enabled bool
}

type GraphConfig struct {
	Profile     Profile
	Scope       GraphScope
	Jobs        outbound.GraphProjectionStore
	Source      outbound.RepositoryGraphSourceStore
	Factory     outbound.GraphSourceReaderFactory
	References  outbound.GraphProjectionReferenceStore
	Generations outbound.RepositoryGraphSourceGenerationStore
	Reference   ReferenceReader
	Enabled     bool
}

type QualificationConfig struct {
	Profile      Profile
	Scope        QualificationScope
	Jobs         outbound.GraphQualificationStore
	Projections  outbound.GraphProjectionStore
	Repairs      outbound.GraphProjectionRepairStore
	Generations  outbound.RepositoryGraphSourceGenerationStore
	Reference    ReferenceReader
	Repositories []domain.ContentHash
	Next         *atomic.Uint64
}

func validProfile(profile Profile) bool {
	return profile == Compatibility || profile == Complete
}

func present(value any) bool {
	if value == nil {
		return false
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return !reflected.IsNil()
	}
	return true
}
