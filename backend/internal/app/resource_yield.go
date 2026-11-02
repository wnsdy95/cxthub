package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const maxParkedRootHeapBytes int64 = 16 << 20
const maxParkedRootSpoolBytes int64 = 64 << 20

type resourceYieldOwnerKey struct{}

type resourceYieldOwner struct {
	permit *resourcePermit
}

func yieldPreparedRoot(ctx context.Context, resources outbound.RootPublicationResources) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, _ := ctx.Value(resourceYieldOwnerKey{}).(*resourceYieldOwner)
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if owner == nil || permit == nil || owner.permit != permit || resources.KeepPermit || resources.HeapBytes <= 0 ||
		resources.HeapBytes > maxParkedRootHeapBytes || resources.SpoolBytes < 0 || resources.SpoolBytes > maxParkedRootSpoolBytes {
		return nil
	}
	gate := permit.owner
	configured, _ := ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	if configured == nil {
		configured = processResources
	}
	if configured != gate {
		return nil
	}
	started := time.Now()
	permit.operations.Lock()
	gate.mu.Lock()
	if gate.activeCapacity() < 2 || !permit.live.Load() || permit.parked || permit.transition || permit.class != ResourceBulk || gate.retained != nil ||
		len(gate.waiters[ResourceBulk]) == 0 || gate.resourceQueueFull(ResourceBulk) {
		gate.mu.Unlock()
		permit.operations.Unlock()
		return nil
	}
	permit.transition = true
	waiter := &resourceWaiter{ready: make(chan struct{}), queued: time.Now(), resume: permit}
	permit.resume = waiter
	gate.waiters[ResourceBulk] = append(gate.waiters[ResourceBulk], waiter)
	gate.mu.Unlock()
	waitCtx := ctx
	if permit.governor != nil {
		var cancel context.CancelFunc
		waitCtx, cancel = gate.processWaitContext(ctx, started)
		defer cancel()
	}
	err := processAdmissionReady(ctx, permit.governor)
	if err == nil && permit.remote != nil {
		err = permit.remote.Park(waitCtx)
	}
	if err != nil {
		gate.mu.Lock()
		gate.removeResourceWaiter(ResourceBulk, waiter)
		permit.resume = nil
		permit.transition = false
		gate.mu.Unlock()
		permit.operations.Unlock()
		return processAdmissionWaitError(ctx, permit.governor, waitCtx, err)
	}
	gate.mu.Lock()
	permit.parked = true
	permit.transition = permit.remote != nil
	gate.retained = permit
	gate.bulk--
	gate.dispatch()
	gate.mu.Unlock()
	permit.operations.Unlock()
	duration := gate.wait
	if permit.governor != nil {
		duration = time.Until(started.Add(gate.wait))
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
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
	permit.operations.Lock()
	defer permit.operations.Unlock()
	gate.mu.Lock()
	if !permit.live.Load() {
		gate.mu.Unlock()
		if err != nil {
			return err
		}
		return domain.ErrResourceOverloaded
	}
	permit.transition = true
	granted := waiter.granted
	if !granted {
		gate.removeResourceWaiter(ResourceBulk, waiter)
	}
	gate.mu.Unlock()
	if err == nil {
		err = processAdmissionReady(ctx, permit.governor)
	}
	if err == nil && permit.remote != nil {
		err = permit.remote.Resume(waitCtx)
		if err == nil {
			err = waitCtx.Err()
		}
		if err == nil {
			err = processAdmissionReady(ctx, permit.governor)
		}
	}
	if err == nil {
		gate.mu.Lock()
		permit.transition = false
		permit.resume = nil
		gate.mu.Unlock()
		return nil
	}
	err = processAdmissionWaitError(ctx, permit.governor, waitCtx, err)
	if granted {
		permit.abandonRemoteBulk(ctx)
	}
	gate.mu.Lock()
	if granted {
		gate.bulk--
		permit.parked = true
		gate.retained = permit
	} else {
		gate.removeResourceWaiter(ResourceBulk, waiter)
	}
	permit.transition = false
	permit.resume = nil
	gate.dispatch()
	gate.mu.Unlock()
	return err
}
