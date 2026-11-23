package systemmaintenance

import (
	"context"
	"errors"
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

// ChunkStore removes document chunks no stored manifest or pending
// finalization job names. One call visits at most one repository; a result
// without a repository means nothing is left to visit for now.
type ChunkStore interface {
	SweepNextDocChunks(context.Context) (domain.DocChunkSweep, error)
}

type RuntimeConfig struct {
	Profile Profile
	Store   RuntimeStore
}

type StorageConfig struct {
	Profile Profile
	Store   StorageStore
}

type ChunksConfig struct {
	Profile Profile
	Store   ChunkStore
}

type Runtime struct {
	store RuntimeStore
}

type Storage struct {
	store StorageStore
}

type Chunks struct {
	store ChunkStore
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

func NewChunks(config ChunksConfig) (*Chunks, error) {
	if !validProfile(config.Profile) || ((config.Store != nil || config.Profile == Complete) && !present(config.Store)) {
		return nil, domain.ErrValidation
	}
	return &Chunks{store: config.Store}, nil
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

// chunkVisits and chunkBudget bound one tick of the chunk cleanup: at most
// this many repositories, and no visit starts or continues past the budget.
const (
	chunkVisits = 16
	chunkBudget = 45 * time.Second
)

// Process visits repositories until the store has none left for now, the
// visit limit is reached or the budget is spent. A repository that cannot be
// swept does not stop the others; its error is returned with the count of
// repositories visited.
func (worker *Chunks) Process(ctx context.Context) (int, error) {
	if worker.store == nil {
		return 0, nil
	}
	tick, cancel := context.WithTimeout(ctx, chunkBudget)
	defer cancel()
	var failures []error
	for visited := 0; visited < chunkVisits; visited++ {
		sweep, err := worker.store.SweepNextDocChunks(tick)
		if err != nil {
			if tick.Err() != nil {
				return visited, errors.Join(append(failures, err)...)
			}
			failures = append(failures, err)
			continue
		}
		if sweep.Repo == "" {
			return visited, errors.Join(failures...)
		}
		if sweep.Removed > 0 || sweep.Deferred > 0 {
			log.Printf("chunk cleanup: repository %s: removed %d orphan chunk(s), freed %d stored byte(s) in %d chunk bodies; %d left for a later visit, %d waiting for the grace period",
				sweep.Repo, sweep.Removed, sweep.Bytes, sweep.Bodies, sweep.Deferred, sweep.Waiting)
		}
	}
	return chunkVisits, errors.Join(failures...)
}

func (worker *Chunks) Run(ctx context.Context) {
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
			if _, err := worker.Process(ctx); err != nil && ctx.Err() == nil {
				log.Printf("chunk cleanup failed: %v", err)
			}
		}
	}
}
