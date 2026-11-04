package documentworker

import (
	"context"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const Lease = 2 * time.Minute
const WorkLimit = 10 * time.Minute
const DisplayJobSteps = 8

type Scope interface {
	WorkerContext(context.Context) context.Context
	WithinWork(context.Context, func(context.Context) error) error
	WithinRepositoryRead(context.Context, domain.ContentHash, func(context.Context) error) error
	CanYield(context.Context) bool
	Yield(context.Context, outbound.RootPublicationResources) error
}

type SourceReader interface {
	OpenSource(context.Context, domain.ContentHash, domain.ContentHash) (documentquery.Source, error)
	OpenViewSource(context.Context, domain.ContentHash, domain.ContentHash) (documentquery.Source, error)
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
