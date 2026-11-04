package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func ownedYieldPermit(t *testing.T, ctx context.Context) (context.Context, *resourcePermit, func()) {
	t.Helper()
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	permit := bound.Value(resourcePermitKey{}).(*resourcePermit)
	return context.WithValue(bound, resourceYieldOwnerKey{}, &resourceYieldOwner{permit: permit}), permit, release
}

func TestPreparedRootYieldRetainsActiveSlotAndServesFIFO(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	bound, permit, release := ownedYieldPermit(t, ctx)
	defer release()
	uploadStarted, uploadFinish := make(chan struct{}), make(chan struct{})
	uploadDone := make(chan error, 1)
	go func() {
		allocated, done, err := AcquireResources(ctx, ResourceBulk)
		if err == nil {
			defer done()
			gate.mu.Lock()
			valid := gate.active == 2 && gate.bulk == 1 && gate.retained == permit && permit.parked
			gate.mu.Unlock()
			if !valid {
				err = errors.New("park did not retain its active reservation")
			}
			if _, _, nested := AcquireResources(bound, ResourceInteractive); !errors.Is(nested, domain.ErrResourceOverloaded) {
				err = errors.New("parked context authorized work")
			}
			called := false
			if nested := WithGraphReadResources(bound, func(context.Context) error { called = true; return nil }); !errors.Is(nested, domain.ErrResourceOverloaded) || called {
				err = errors.New("parked context authorized graph read")
			}
			other := allocated.Value(resourcePermitKey{}).(*resourcePermit)
			otherCtx := context.WithValue(allocated, resourceYieldOwnerKey{}, &resourceYieldOwner{permit: other})
			if nested := yieldPreparedRoot(otherCtx, outbound.RootPublicationResources{HeapBytes: 1}); nested != nil {
				err = nested
			}
			gate.mu.Lock()
			if gate.retained != permit || other.parked {
				err = errors.New("second job parked while both active slots were occupied")
			}
			gate.mu.Unlock()
			close(uploadStarted)
			<-uploadFinish
		}
		uploadDone <- err
	}()
	waitResourceQueue(t, gate, ResourceBulk, 1)
	yielded := make(chan error, 1)
	go func() {
		yielded <- yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: maxParkedRootHeapBytes, SpoolBytes: maxParkedRootSpoolBytes})
	}()
	select {
	case <-uploadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("queued upload did not run before completion")
	}
	close(uploadFinish)
	if err := <-uploadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-yielded; err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	resumed := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked
	gate.mu.Unlock()
	if !resumed {
		t.Fatal("resume allocated a second active slot")
	}
	if permit.resumeWait.Load() <= 0 {
		t.Fatal("resume queue wait not recorded")
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestPreparedRootResumeUsesReservedSlotAtCapacity(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	bound, permit, release := ownedYieldPermit(t, ctx)
	defer release()
	_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead()
	gate.mu.Lock()
	permit.parked = true
	gate.retained, gate.bulk = permit, 0
	waiter := &resourceWaiter{ready: make(chan struct{}), queued: time.Now(), resume: permit}
	gate.waiters[ResourceBulk] = append(gate.waiters[ResourceBulk], waiter)
	gate.dispatch()
	valid := waiter.granted && gate.active == 2 && gate.bulk == 1 && gate.retained == nil && !permit.parked
	gate.mu.Unlock()
	if !valid {
		t.Fatal("resume could not use its already reserved slot")
	}
	if _, nested, err := AcquireResources(bound, ResourceBulk); err != nil {
		t.Fatal(err)
	} else {
		nested()
	}
	releaseRead()
	release()
	assertResourcesReleased(t, gate)
}

func TestPreparedRootYieldKeepsPermitUntilFlagCleared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate := admissionTestContext(time.Minute)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		bound, permit, release := ownedYieldPermit(t, ctx)
		defer release()
		started, finish := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(finish) })
		defer unblock()
		queued := make(chan error, 1)
		go func() {
			_, done, err := AcquireResources(ctx, ResourceBulk)
			if err == nil {
				defer done()
				gate.mu.Lock()
				retained := gate.active == 2 && gate.bulk == 1 && gate.retained == permit && permit.parked && permit.live.Load()
				gate.mu.Unlock()
				if !retained {
					err = errors.New("cleared keep-permit flag lost the retained reservation")
				}
				close(started)
				<-finish
			}
			queued <- err
		}()
		synctest.Wait()
		gate.mu.Lock()
		queuedBeforeYield := len(gate.waiters[ResourceBulk]) == 1
		gate.mu.Unlock()
		if !queuedBeforeYield {
			t.Fatal("foreground work did not queue behind the live bulk permit")
		}
		resources := outbound.RootPublicationResources{HeapBytes: 1, SpoolBytes: 1, KeepPermit: true}
		yielded := make(chan error, 1)
		go func() { yielded <- yieldPreparedRoot(bound, resources) }()
		synctest.Wait()
		select {
		case err := <-yielded:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("keep-permit yield waited for queued work")
		}
		canceled, cancelYield := context.WithCancel(bound)
		cancelYield()
		if err := yieldPreparedRoot(canceled, resources); !errors.Is(err, context.Canceled) {
			t.Fatal("keep-permit flag suppressed context cancellation", err)
		}
		gate.mu.Lock()
		live := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked &&
			permit.live.Load() && permit.resumeWait.Load() == 0 && len(gate.waiters[ResourceBulk]) == 1
		gate.mu.Unlock()
		if !live {
			t.Fatal("keep-permit flag changed the live reservation or queued work")
		}
		select {
		case <-started:
			t.Fatal("queued foreground work ran while keep-permit was set")
		default:
		}
		resources.KeepPermit = false
		go func() { yielded <- yieldPreparedRoot(bound, resources) }()
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("clearing keep-permit did not restore parking")
		}
		select {
		case err := <-yielded:
			t.Fatal("parked work resumed before foreground release", err)
		default:
		}
		time.Sleep(time.Millisecond)
		unblock()
		if err := <-queued; err != nil {
			t.Fatal(err)
		}
		if err := <-yielded; err != nil {
			t.Fatal(err)
		}
		gate.mu.Lock()
		resumed := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked && permit.live.Load()
		gate.mu.Unlock()
		if !resumed || permit.resumeWait.Load() <= 0 {
			t.Fatal("cleared keep-permit flag did not resume the owned reservation")
		}
		release()
		assertResourcesReleased(t, gate)
	})
}

func TestPreparedRootYieldDeclinesIneligibleReservations(t *testing.T) {
	for _, reason := range []string{"borrowed", "foreign", "zero", "negative", "heap", "disk", "full"} {
		t.Run(reason, func(t *testing.T) {
			ctx, gate := admissionTestContext(time.Second)
			bound, permit, release := ownedYieldPermit(t, ctx)
			defer release()
			resources := outbound.RootPublicationResources{HeapBytes: maxParkedRootHeapBytes, SpoolBytes: maxParkedRootSpoolBytes}
			switch reason {
			case "borrowed":
				bound = context.WithValue(bound, resourceYieldOwnerKey{}, (*resourceYieldOwner)(nil))
			case "foreign":
				bound = context.WithValue(bound, resourceAdmissionKey{}, &resourceAdmission{})
			case "zero":
				resources.HeapBytes = 0
			case "negative":
				resources.SpoolBytes = -1
			case "heap":
				resources.HeapBytes++
			case "disk":
				resources.SpoolBytes++
			}
			count := 1
			if reason == "full" {
				count = resourceQueueLimit
			}
			gate.mu.Lock()
			for range count {
				gate.waiters[ResourceBulk] = append(gate.waiters[ResourceBulk], &resourceWaiter{ready: make(chan struct{}), queued: time.Now()})
			}
			gate.mu.Unlock()
			if err := yieldPreparedRoot(bound, resources); err != nil {
				t.Fatal(err)
			}
			gate.mu.Lock()
			unchanged := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked && len(gate.waiters[ResourceBulk]) == count
			gate.waiters[ResourceBulk] = nil
			gate.mu.Unlock()
			if !unchanged {
				t.Fatal("ineligible yield changed admission state")
			}
			release()
			assertResourcesReleased(t, gate)
		})
	}
}

func TestPreparedRootYieldGrantCancellationRace(t *testing.T) {
	for range 40 {
		ctx, gate := admissionTestContext(time.Second)
		bound, _, release := ownedYieldPermit(t, ctx)
		bound, cancel := context.WithCancel(bound)
		started, finish, uploadDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			_, releaseUpload, err := AcquireResources(ctx, ResourceBulk)
			if err == nil {
				close(started)
				<-finish
				releaseUpload()
			}
			uploadDone <- err
		}()
		waitResourceQueue(t, gate, ResourceBulk, 1)
		result := make(chan error, 1)
		go func() { result <- yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}) }()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			cancel()
			close(finish)
			t.Fatal("upload did not acquire admission")
		}
		go cancel()
		close(finish)
		if err := <-result; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := <-uploadDone; err != nil {
			t.Fatal(err)
		}
		cancel()
		release()
		assertResourcesReleased(t, gate)
	}
}

func TestPreparedRootYieldCancellationKeepsCleanupReservation(t *testing.T) {
	for _, reason := range []string{"cancel", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			ctx, gate := admissionTestContext(40 * time.Millisecond)
			bound, permit, release := ownedYieldPermit(t, ctx)
			defer release()
			bound, cancel := context.WithCancel(bound)
			defer cancel()
			started, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(finish) }) }
			defer unblock()
			go func() {
				_, releaseUpload, err := AcquireResources(ctx, ResourceBulk)
				if err == nil {
					close(started)
					<-finish
					releaseUpload()
				}
				done <- err
			}()
			waitResourceQueue(t, gate, ResourceBulk, 1)
			result := make(chan error, 1)
			go func() { result <- yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upload did not acquire yielded admission")
			}
			want := domain.ErrResourceOverloaded
			if reason == "cancel" {
				cancel()
				want = context.Canceled
			}
			if err := <-result; !errors.Is(err, want) {
				t.Fatal("wrong resume failure", err)
			}
			gate.mu.Lock()
			reserved := gate.active == 2 && gate.bulk == 1 && gate.retained == permit && permit.parked && len(gate.waiters[ResourceBulk]) == 0
			gate.mu.Unlock()
			if !reserved {
				t.Fatal("resume failure freed memory before cleanup")
			}
			release()
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertResourcesReleased(t, gate)
		})
	}
}

type cancelAfterResumeContext struct {
	context.Context
	permit *resourcePermit
	cancel context.CancelFunc
}

func (ctx cancelAfterResumeContext) Err() error {
	if ctx.permit.resumeWait.Load() > 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestPreparedRootYieldCancelAfterGrantRetainsCleanupSlot(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	bound, permit, release := ownedYieldPermit(t, ctx)
	defer release()
	bound, cancel := context.WithCancel(bound)
	defer cancel()
	bound = cancelAfterResumeContext{Context: bound, permit: permit, cancel: cancel}
	uploadDone := make(chan error, 1)
	go func() {
		_, releaseUpload, err := AcquireResources(ctx, ResourceBulk)
		if err == nil {
			releaseUpload()
		}
		uploadDone <- err
	}()
	waitResourceQueue(t, gate, ResourceBulk, 1)
	if err := yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal("post-grant cancellation was not returned", err)
	}
	if err := <-uploadDone; err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	reserved := gate.active == 1 && gate.bulk == 0 && gate.retained == permit && permit.parked && len(gate.waiters[ResourceBulk]) == 0
	gate.mu.Unlock()
	if !reserved || permit.resumeWait.Load() <= 0 {
		t.Fatal("granted cancellation lost retained cleanup reservation")
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestPreparedRootYieldPreservesPrecedingAndFollowingFIFO(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bound, _, release := ownedYieldPermit(t, ctx)
	defer release()
	order := make(chan string, 4)
	done := make(chan error, 4)
	firstFinish := make(chan struct{})
	var finishOnce sync.Once
	finishFirst := func() { finishOnce.Do(func() { close(firstFinish) }) }
	defer finishFirst()
	start := func(name string) {
		go func() {
			done <- resourceWork(ctx, ResourceBulk, func(context.Context) error {
				order <- name
				if name == "first" {
					<-firstFinish
				}
				return nil
			})
		}()
	}
	start("first")
	waitResourceQueue(t, gate, ResourceBulk, 1)
	start("second")
	waitResourceQueue(t, gate, ResourceBulk, 2)
	go func() {
		err := yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1})
		if err == nil {
			order <- "resume"
		}
		release()
		done <- err
	}()
	select {
	case actual := <-order:
		if actual != "first" {
			t.Fatal("wrong first waiter", actual)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first waiter did not run")
	}
	start("last")
	waitResourceQueue(t, gate, ResourceBulk, 3)
	finishFirst()
	for _, expected := range []string{"second", "resume", "last"} {
		select {
		case actual := <-order:
			if actual != expected {
				t.Fatal("FIFO changed", expected, actual)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("waiter did not complete", expected)
		}
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	assertResourcesReleased(t, gate)
}
