package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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
	operations sync.Mutex
	owner      *resourceAdmission
	class      ResourceClass
	live       atomic.Bool
	wait       time.Duration
	parked     bool
	resumeWait atomic.Int64
	resume     *resourceWaiter
	transition bool
	governor   outbound.ProcessAdmission
	remote     outbound.ProcessAdmissionPermit
	cancel     context.CancelFunc
	stop       func() bool
}

type resourceWaiter struct {
	ready   chan struct{}
	granted bool
	queued  time.Time
	wait    time.Duration
	resume  *resourcePermit
}

type resourceAdmission struct {
	mu            sync.Mutex
	active        int
	bulk          int
	waiters       [2][]*resourceWaiter
	wait          time.Duration
	retained      *resourcePermit
	limits        ProcessAdmissionLimits
	governor      outbound.ProcessAdmission
	configuration *processAdmissionConfiguration
}

var processResources = &resourceAdmission{wait: resourceWaitLimit}

func AcquireResources(ctx context.Context, class ResourceClass) (context.Context, func(), error) {
	return acquireResources(ctx, class, true)
}

func acquireResources(ctx context.Context, class ResourceClass, wait bool) (context.Context, func(), error) {
	gate, _ := ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	if gate == nil {
		gate = processResources
	}
	return gate.acquireWithWait(ctx, class, wait)
}

func (gate *resourceAdmission) acquire(ctx context.Context, class ResourceClass) (context.Context, func(), error) {
	return gate.acquireWithWait(ctx, class, true)
}

func (gate *resourceAdmission) acquireWithWait(ctx context.Context, class ResourceClass, wait bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if class != ResourceInteractive && class != ResourceBulk {
		return ctx, nil, domain.ErrValidation
	}
	if reused, err := gate.reuseProcessPermit(ctx, class, wait); reused {
		if err != nil {
			return ctx, nil, err
		}
		return ctx, func() {}, nil
	}
	started := time.Now()
	gate.mu.Lock()
	governor, configuration := gate.governor, gate.configuration
	closing := configuration != nil && configuration.restoring
	gate.mu.Unlock()
	if closing {
		return ctx, nil, domain.ErrResourceOverloaded
	}
	if err := processAdmissionReady(ctx, governor); err != nil {
		return ctx, nil, err
	}
	workCtx := ctx
	var cancel context.CancelFunc
	var stop func() bool
	if governor != nil {
		workCtx, cancel = context.WithCancel(ctx)
		stop = context.AfterFunc(governor.Context(), cancel)
	}
	transferred := false
	defer func() {
		if !transferred && cancel != nil {
			stop()
			cancel()
		}
	}()
	waitCtx := workCtx
	if governor != nil {
		var cancelWait context.CancelFunc
		waitCtx, cancelWait = context.WithDeadline(workCtx, started.Add(gate.wait))
		defer cancelWait()
	}
	waiter := &resourceWaiter{ready: make(chan struct{})}
	gate.mu.Lock()
	if gate.configuration != configuration || (configuration != nil && configuration.restoring) || (governor == nil && gate.resourceQueueFull(class)) {
		gate.mu.Unlock()
		return ctx, nil, domain.ErrResourceOverloaded
	}
	waiter.queued = time.Now()
	gate.waiters[class] = append(gate.waiters[class], waiter)
	gate.dispatch()
	granted := waiter.granted
	if governor != nil && !granted && len(gate.waiters[0])+len(gate.waiters[1]) > gate.queueCapacity() {
		gate.removeResourceWaiter(class, waiter)
		gate.mu.Unlock()
		return ctx, nil, domain.ErrResourceOverloaded
	}
	if !granted && !wait {
		last := len(gate.waiters[class]) - 1
		gate.waiters[class][last] = nil
		gate.waiters[class] = gate.waiters[class][:last]
		gate.mu.Unlock()
		return ctx, nil, domain.ErrResourceOverloaded
	}
	gate.mu.Unlock()
	if !granted {
		duration := gate.wait
		if governor != nil {
			duration = time.Until(started.Add(gate.wait))
		}
		timer := time.NewTimer(duration)
		defer timer.Stop()
		var err error
		select {
		case <-waiter.ready:
		case <-workCtx.Done():
			err = workCtx.Err()
		case <-timer.C:
			err = domain.ErrResourceOverloaded
		}
		if workCtx.Err() != nil {
			err = workCtx.Err()
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
			gate.restoreProcessAdmission()
			gate.mu.Unlock()
			return ctx, nil, processAdmissionWaitError(ctx, governor, waitCtx, err)
		}
	}
	permit := &resourcePermit{owner: gate, class: class, wait: waiter.wait, governor: governor, cancel: cancel, stop: stop}
	permit.live.Store(true)
	transferred = true
	if governor != nil {
		if err := processAdmissionReady(ctx, governor); err != nil {
			permit.release()
			return ctx, nil, err
		}
		remote, err := governor.Acquire(waitCtx, class == ResourceBulk, wait)
		permit.remote = remote
		if err == nil && remote == nil {
			err = domain.ErrResourceOverloaded
		}
		if err == nil {
			err = processAdmissionReady(ctx, governor)
		}
		if err == nil {
			err = waitCtx.Err()
		}
		if err != nil {
			failure := processAdmissionWaitError(ctx, governor, waitCtx, err)
			permit.release()
			return ctx, nil, failure
		}
		permit.wait = time.Since(started)
	}
	return context.WithValue(workCtx, resourcePermitKey{}, permit), permit.release, nil
}

func (gate *resourceAdmission) dispatch() {
	for {
		class := ResourceBulk
		if gate.bulk != 0 || len(gate.waiters[class]) == 0 {
			class = ResourceInteractive
			if gate.active >= gate.activeCapacity() || len(gate.waiters[class]) == 0 {
				return
			}
		}
		waiter := gate.waiters[class][0]
		if waiter.resume == nil && gate.active >= gate.activeCapacity() {
			return
		}
		gate.waiters[class][0] = nil
		gate.waiters[class] = gate.waiters[class][1:]
		waiter.granted = true
		waiter.wait = time.Since(waiter.queued)
		if waiter.resume == nil {
			gate.active++
		} else {
			waiter.resume.parked = false
			waiter.resume.resumeWait.Add(int64(waiter.wait))
			gate.retained = nil
		}
		if class == ResourceBulk {
			gate.bulk++
		}
		close(waiter.ready)
	}
}

func resourceWork(ctx context.Context, class ResourceClass, work func(context.Context) error) error {
	return resourceWorkWithWait(ctx, class, true, work)
}

func resourceWorkWithWait(ctx context.Context, class ResourceClass, wait bool, work func(context.Context) error) error {
	bound, release, err := acquireResources(ctx, class, wait)
	if err != nil {
		return err
	}
	defer release()
	return work(bound)
}

func admittedJob[Job any](ctx context.Context, claim func(context.Context) (Job, error), run func(context.Context, Job) error) error {
	return admittedWork(ctx, func(bound context.Context) error {
		job, err := claim(bound)
		if err != nil {
			return err
		}
		return run(bound, job)
	})
}

func admittedWork(ctx context.Context, work func(context.Context) error) error {
	return resourceWork(ctx, ResourceBulk, func(bound context.Context) error {
		var owner *resourceYieldOwner
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if prior, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit); prior != permit {
			owner = &resourceYieldOwner{permit: permit}
		}
		bound = context.WithValue(bound, resourceYieldOwnerKey{}, owner)
		return work(bound)
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
