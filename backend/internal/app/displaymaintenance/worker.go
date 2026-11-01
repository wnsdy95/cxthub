package displaymaintenance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Scope interface {
	SystemContext(context.Context) context.Context
	AcquireBulk(context.Context) (context.Context, func(), error)
}

type Worker struct {
	storage outbound.DocDisplayMaintenanceStore
	scope   Scope
}

func New(storage outbound.DocDisplayMaintenanceStore, scope Scope) (*Worker, error) {
	if missingDependency(storage) {
		return nil, fmt.Errorf("%w: document display maintenance storage is required", domain.ErrValidation)
	}
	if missingDependency(scope) {
		return nil, fmt.Errorf("%w: document display maintenance scope is required", domain.ErrValidation)
	}
	return &Worker{storage: storage, scope: scope}, nil
}

func missingDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (worker *Worker) Process(ctx context.Context, limit int) (int, error) {
	if outbound.DocReadOnly(ctx) {
		return 0, domain.ErrForbidden
	}
	if limit <= 0 {
		return 0, nil
	}
	ctx = worker.scope.SystemContext(ctx)
	for processed := 0; processed < limit; processed++ {
		err := worker.step(ctx)
		if errors.Is(err, domain.ErrNotFound) {
			return processed, nil
		}
		if err != nil {
			return processed, err
		}
	}
	return limit, nil
}

func (worker *Worker) step(ctx context.Context) error {
	bound, release, err := worker.scope.AcquireBulk(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = worker.storage.StepDocDisplayMaintenance(bound)
	return err
}

func (worker *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		processed, err := worker.Process(ctx, 1)
		if err != nil && ctx.Err() == nil {
			log.Printf("document display maintenance: %v", err)
		}
		if processed > 0 && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
