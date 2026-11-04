package systemmaintenance

import (
	"context"
	"log"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type Profile string

const (
	Compatibility Profile = "compatibility"
	Complete      Profile = "complete"
)

type RuntimeStore interface {
	PruneRuntimeState(context.Context, time.Time) error
}

type StorageStore interface {
	ReconcileNextStorageUsage(context.Context) (bool, error)
}

type RuntimeConfig struct {
	Profile Profile
	Store   RuntimeStore
}

type StorageConfig struct {
	Profile Profile
	Store   StorageStore
}

type Runtime struct {
	store RuntimeStore
}

type Storage struct {
	store StorageStore
}

func NewRuntime(config RuntimeConfig) (*Runtime, error) {
	if !validProfile(config.Profile) || ((config.Store != nil || config.Profile == Complete) && !present(config.Store)) {
		return nil, domain.ErrValidation
	}
	return &Runtime{store: config.Store}, nil
}

func NewStorage(config StorageConfig) (*Storage, error) {
	if !validProfile(config.Profile) || ((config.Store != nil || config.Profile == Complete) && !present(config.Store)) {
		return nil, domain.ErrValidation
	}
	return &Storage{store: config.Store}, nil
}

func validProfile(profile Profile) bool {
	return profile == Compatibility || profile == Complete
}

func present(dependency any) bool {
	if dependency == nil {
		return false
	}
	switch reflected := reflect.ValueOf(dependency); reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !reflected.IsNil()
	}
	return true
}

func (worker *Runtime) Process(ctx context.Context) error {
	if worker.store == nil {
		return nil
	}
	prune, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return worker.store.PruneRuntimeState(prune, time.Now().UTC())
}

func (worker *Runtime) Run(ctx context.Context) {
	if worker.store == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := worker.Process(ctx); err != nil && ctx.Err() == nil {
				log.Printf("runtime state cleanup: %v", err)
			}
		}
	}
}

func (worker *Storage) Process(ctx context.Context) (int, error) {
	if worker.store == nil {
		return 0, nil
	}
	for processed := 0; processed < 8; processed++ {
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		did, err := worker.store.ReconcileNextStorageUsage(call)
		cancel()
		if err != nil {
			return processed, err
		}
		if !did {
			return processed, nil
		}
	}
	return 8, nil
}

func (worker *Storage) Run(ctx context.Context) {
	if worker.store == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := worker.Process(ctx); err != nil {
				log.Printf("storage reconciliation failed: %v", err)
			}
		}
	}
}
