package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ProcessAdmissionLimits struct {
	Active int
	Queue  int
}

type processAdmissionConfiguration struct {
	previous  ProcessAdmissionLimits
	restoring bool
}

func ConfigureProcessAdmission(governor outbound.ProcessAdmission, limits ProcessAdmissionLimits) (func(), error) {
	if !rootAdapterPresent(governor) || governor.Context() == nil || limits.Active < 0 || limits.Active > 2 || limits.Queue < 0 {
		return nil, domain.ErrValidation
	}
	gate := processResources
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.configuration != nil || gate.active != 0 || gate.bulk != 0 || gate.retained != nil || len(gate.waiters[0])+len(gate.waiters[1]) != 0 {
		return nil, domain.ErrConflict
	}
	configuration := &processAdmissionConfiguration{previous: gate.limits}
	gate.configuration, gate.governor, gate.limits = configuration, governor, limits
	return func() {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		if gate.configuration != configuration {
			return
		}
		configuration.restoring = true
		gate.restoreProcessAdmission()
	}, nil
}

func (gate *resourceAdmission) restoreProcessAdmission() {
	configuration := gate.configuration
	if configuration == nil || !configuration.restoring || gate.active != 0 || gate.bulk != 0 || gate.retained != nil || len(gate.waiters[0])+len(gate.waiters[1]) != 0 {
		return
	}
	gate.limits = configuration.previous
	gate.governor, gate.configuration = nil, nil
}

func (gate *resourceAdmission) activeCapacity() int {
	if gate.limits.Active == 0 {
		return 2
	}
	return gate.limits.Active
}

func (gate *resourceAdmission) queueCapacity() int {
	if gate.limits.Queue == 0 {
		return resourceQueueLimit
	}
	return gate.limits.Queue
}

func (gate *resourceAdmission) resourceQueueFull(class ResourceClass) bool {
	queued := len(gate.waiters[class])
	if gate.governor != nil {
		queued = len(gate.waiters[0]) + len(gate.waiters[1])
	}
	return queued >= gate.queueCapacity()
}

func processAdmissionReady(ctx context.Context, governor outbound.ProcessAdmission) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if governor == nil {
		return nil
	}
	if governor.Context().Err() != nil {
		if err := governor.Err(); err != nil {
			return err
		}
		return domain.ErrResourceOverloaded
	}
	if !governor.Ready() {
		return domain.ErrResourceOverloaded
	}
	return nil
}

func processAdmissionWaitError(ctx context.Context, governor outbound.ProcessAdmission, waitCtx context.Context, failure error) error {
	if err := processAdmissionReady(ctx, governor); err != nil {
		return err
	}
	if governor != nil && waitCtx.Err() != nil {
		return domain.ErrResourceOverloaded
	}
	return failure
}

func (gate *resourceAdmission) reuseProcessPermit(ctx context.Context, class ResourceClass, wait bool) (bool, error) {
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if permit == nil || permit.owner != gate {
		return false, nil
	}
	started := time.Now()
	if !wait {
		if !permit.operations.TryLock() {
			return true, domain.ErrResourceOverloaded
		}
	} else {
		permit.operations.Lock()
	}
	defer permit.operations.Unlock()
	gate.mu.Lock()
	if !permit.live.Load() {
		gate.mu.Unlock()
		return false, nil
	}
	if permit.parked || permit.transition {
		gate.mu.Unlock()
		return true, domain.ErrResourceOverloaded
	}
	upgrade := class == ResourceBulk && permit.class == ResourceInteractive
	if upgrade && (gate.bulk != 0 || len(gate.waiters[ResourceBulk]) != 0) {
		gate.mu.Unlock()
		return true, domain.ErrResourceOverloaded
	}
	if upgrade {
		permit.class = ResourceBulk
		gate.bulk++
	}
	gate.mu.Unlock()
	err := processAdmissionReady(ctx, permit.governor)
	if err == nil && upgrade && permit.remote != nil {
		waitCtx, cancel := gate.processWaitContext(ctx, started)
		err = permit.remote.Upgrade(waitCtx, wait)
		upgraded := err == nil
		if err == nil {
			err = waitCtx.Err()
		}
		err = processAdmissionWaitError(ctx, permit.governor, waitCtx, err)
		cancel()
		if err != nil && upgraded {
			permit.abandonRemoteBulk(ctx)
		}
	}
	if err != nil && upgrade {
		gate.mu.Lock()
		permit.class = ResourceInteractive
		gate.bulk--
		gate.dispatch()
		gate.mu.Unlock()
	}
	return true, err
}

func (permit *resourcePermit) release() {
	if permit.cancel != nil {
		permit.stop()
		permit.cancel()
	}
	permit.operations.Lock()
	defer permit.operations.Unlock()
	if !permit.live.CompareAndSwap(true, false) {
		return
	}
	if permit.remote != nil {
		_ = permit.remote.Release()
		permit.remote = nil
	}
	gate := permit.owner
	gate.mu.Lock()
	gate.active--
	if permit.parked {
		if gate.retained == permit {
			gate.retained = nil
		}
	} else if permit.class == ResourceBulk {
		gate.bulk--
	}
	if permit.resume != nil && !permit.resume.granted {
		gate.removeResourceWaiter(ResourceBulk, permit.resume)
		close(permit.resume.ready)
	}
	permit.resume = nil
	gate.dispatch()
	gate.restoreProcessAdmission()
	gate.mu.Unlock()
}

func (gate *resourceAdmission) removeResourceWaiter(class ResourceClass, waiter *resourceWaiter) {
	for position, pending := range gate.waiters[class] {
		if pending == waiter {
			gate.waiters[class] = append(gate.waiters[class][:position], gate.waiters[class][position+1:]...)
			return
		}
	}
}

func (permit *resourcePermit) abandonRemoteBulk(ctx context.Context) {
	if permit.remote == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), resourceWaitLimit)
	defer cancel()
	if err := permit.remote.Park(cleanup); err != nil {
		_ = permit.remote.Release()
		permit.remote = nil
		if permit.cancel != nil {
			permit.cancel()
		}
	}
}

func (gate *resourceAdmission) processWaitContext(ctx context.Context, started time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, started.Add(gate.wait))
}
