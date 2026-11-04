package documentquery

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type FindFieldsReader interface {
	ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error)
}

type FindFieldsDemand interface {
	EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type FindFields interface {
	ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error)
	EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type FindDisplayDemand interface {
	EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error)
}

type FindDependencies struct {
	Fields       FindFields
	FieldsRead   FindFieldsReader
	FieldsDemand FindFieldsDemand
	DisplayRead  outbound.DocDisplayReader
	Display      FindDisplayDemand
	EventDisplay outbound.DocEventDisplayReader
}

func (dependencies FindDependencies) validate(readOnly bool) error {
	for _, optional := range []any{dependencies.Fields, dependencies.FieldsRead, dependencies.FieldsDemand, dependencies.DisplayRead, dependencies.Display, dependencies.EventDisplay} {
		if optional != nil && absent(optional) {
			return fmt.Errorf("%w: typed-nil document find dependency", domain.ErrValidation)
		}
	}
	if readOnly && (dependencies.Fields != nil || dependencies.FieldsDemand != nil || dependencies.Display != nil) {
		return fmt.Errorf("%w: read-only document find cannot retain preparation dependencies", domain.ErrValidation)
	}
	return nil
}

func (queries *Queries) findFieldsRead() FindFieldsReader {
	if queries.find.FieldsRead != nil {
		return queries.find.FieldsRead
	}
	return queries.find.Fields
}

func (queries *Queries) findFieldsDemand() FindFieldsDemand {
	if queries.find.FieldsDemand != nil {
		return queries.find.FieldsDemand
	}
	return queries.find.Fields
}

func (queries *Queries) findInteractiveWork(ctx context.Context, work func(context.Context) error) error {
	bound, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work(bound)
}
