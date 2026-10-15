package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ResourceClass uint8

const (
	ResourceInteractive ResourceClass = iota
	ResourceBulk
)

const resourceQueueLimit = 8
const resourceWaitLimit = time.Second

type resourcePermitKey struct{}
type resourceAdmissionKey struct{}

type resourcePermit struct {
	owner *resourceAdmission
	class ResourceClass
	live  atomic.Bool
}

type resourceWaiter struct {
	ready   chan struct{}
	granted bool
}

type resourceAdmission struct {
	mu      sync.Mutex
	active  int
	bulk    int
	waiters [2][]*resourceWaiter
	wait    time.Duration
}

var processResources = &resourceAdmission{wait: resourceWaitLimit}

func AcquireResources(ctx context.Context, class ResourceClass) (context.Context, func(), error) {
	gate, _ := ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	if gate == nil {
		gate = processResources
	}
	return gate.acquire(ctx, class)
}

func (gate *resourceAdmission) acquire(ctx context.Context, class ResourceClass) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if class != ResourceInteractive && class != ResourceBulk {
		return ctx, nil, domain.ErrValidation
	}
	if permit, ok := ctx.Value(resourcePermitKey{}).(*resourcePermit); ok && permit.owner == gate {
		gate.mu.Lock()
		if permit.live.Load() {
			if class == ResourceBulk && permit.class == ResourceInteractive {
				if gate.bulk != 0 || len(gate.waiters[ResourceBulk]) != 0 {
					gate.mu.Unlock()
					return ctx, nil, domain.ErrResourceOverloaded
				}
				permit.class = ResourceBulk
				gate.bulk++
			}
			gate.mu.Unlock()
			return ctx, func() {}, nil
		}
		gate.mu.Unlock()
	}
	waiter := &resourceWaiter{ready: make(chan struct{})}
	gate.mu.Lock()
	if len(gate.waiters[class]) >= resourceQueueLimit {
		gate.mu.Unlock()
		return ctx, nil, domain.ErrResourceOverloaded
	}
	gate.waiters[class] = append(gate.waiters[class], waiter)
	gate.dispatch()
	granted := waiter.granted
	gate.mu.Unlock()
	if !granted {
		timer := time.NewTimer(gate.wait)
		defer timer.Stop()
		var err error
		select {
		case <-waiter.ready:
		case <-ctx.Done():
			err = ctx.Err()
		case <-timer.C:
			err = domain.ErrResourceOverloaded
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			gate.mu.Lock()
			if waiter.granted {
				gate.active--
				if class == ResourceBulk {
					gate.bulk--
				}
			} else {
				for index, queued := range gate.waiters[class] {
					if queued == waiter {
						gate.waiters[class] = append(gate.waiters[class][:index], gate.waiters[class][index+1:]...)
						break
					}
				}
			}
			gate.dispatch()
			gate.mu.Unlock()
			return ctx, nil, err
		}
	}
	permit := &resourcePermit{owner: gate, class: class}
	permit.live.Store(true)
	release := func() {
		if !permit.live.CompareAndSwap(true, false) {
			return
		}
		gate.mu.Lock()
		gate.active--
		if permit.class == ResourceBulk {
			gate.bulk--
		}
		gate.dispatch()
		gate.mu.Unlock()
	}
	return context.WithValue(ctx, resourcePermitKey{}, permit), release, nil
}

func (gate *resourceAdmission) dispatch() {
	for gate.active < 2 {
		class := ResourceInteractive
		if len(gate.waiters[class]) == 0 {
			class = ResourceBulk
			if gate.bulk != 0 || len(gate.waiters[class]) == 0 {
				return
			}
		}
		waiter := gate.waiters[class][0]
		gate.waiters[class][0] = nil
		gate.waiters[class] = gate.waiters[class][1:]
		waiter.granted = true
		gate.active++
		if class == ResourceBulk {
			gate.bulk++
		}
		close(waiter.ready)
	}
}

func resourceWork(ctx context.Context, class ResourceClass, work func(context.Context) error) error {
	bound, release, err := AcquireResources(ctx, class)
	if err != nil {
		return err
	}
	defer release()
	return work(bound)
}

func admittedJob[Job any](ctx context.Context, claim func(context.Context) (Job, error), run func(context.Context, Job) error) error {
	return resourceWork(ctx, ResourceBulk, func(bound context.Context) error {
		job, err := claim(bound)
		if err != nil {
			return err
		}
		return run(bound, job)
	})
}

func resourceReadForRepo[Result any](ctx context.Context, service *Service, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		var zero Result
		return zero, err
	}
	defer release()
	return repositoryReadForRepo(bound, service, repo, read)
}
