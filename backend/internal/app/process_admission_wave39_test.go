package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave39ProcessGovernor struct {
	ctx          context.Context
	cancel       context.CancelFunc
	ready        atomic.Bool
	acquisitions atomic.Int64
	acquire      func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error)
}

func newWave39ProcessGovernor(t *testing.T) *wave39ProcessGovernor {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	governor := &wave39ProcessGovernor{ctx: ctx, cancel: cancel}
	governor.ready.Store(true)
	t.Cleanup(cancel)
	return governor
}

func (governor *wave39ProcessGovernor) Acquire(ctx context.Context, bulk, wait bool) (outbound.ProcessAdmissionPermit, error) {
	governor.acquisitions.Add(1)
	if governor.acquire != nil {
		return governor.acquire(ctx, bulk, wait)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &wave39ProcessPermit{}, nil
}

func (governor *wave39ProcessGovernor) Context() context.Context { return governor.ctx }
func (governor *wave39ProcessGovernor) Ready() bool {
	return governor.ready.Load() && governor.ctx.Err() == nil
}
func (governor *wave39ProcessGovernor) Err() error {
	if governor.ctx.Err() != nil {
		return domain.ErrResourceOverloaded
	}
	return nil
}
func (governor *wave39ProcessGovernor) Close() error { governor.cancel(); return nil }

type wave39ProcessPermit struct {
	upgrades atomic.Int64
	parks    atomic.Int64
	resumes  atomic.Int64
	releases atomic.Int64
	upgrade  func(context.Context, bool) error
	park     func(context.Context) error
	resume   func(context.Context) error
	release  func() error
}

func (permit *wave39ProcessPermit) Upgrade(ctx context.Context, wait bool) error {
	permit.upgrades.Add(1)
	if permit.upgrade != nil {
		return permit.upgrade(ctx, wait)
	}
	return ctx.Err()
}
func (permit *wave39ProcessPermit) Park(ctx context.Context) error {
	permit.parks.Add(1)
	if permit.park != nil {
		return permit.park(ctx)
	}
	return ctx.Err()
}
func (permit *wave39ProcessPermit) Resume(ctx context.Context) error {
	permit.resumes.Add(1)
	if permit.resume != nil {
		return permit.resume(ctx)
	}
	return ctx.Err()
}
func (permit *wave39ProcessPermit) Release() error {
	permit.releases.Add(1)
	if permit.release != nil {
		return permit.release()
	}
	return nil
}

func wave39ProcessContext(t *testing.T, active int) (context.Context, *resourceAdmission, *wave39ProcessGovernor) {
	t.Helper()
	governor := newWave39ProcessGovernor(t)
	gate := &resourceAdmission{wait: resourceWaitLimit, governor: governor, limits: ProcessAdmissionLimits{Active: active}}
	return context.WithValue(context.Background(), resourceAdmissionKey{}, gate), gate, governor
}

func TestWave39ProcessAdmissionTypedNilRejected(t *testing.T) {
	var governor *wave39ProcessGovernor
	if restore, err := ConfigureProcessAdmission(governor, ProcessAdmissionLimits{}); restore != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("typed-nil process admission accepted", err)
	}
}

func TestWave39ProcessAdmissionConfigurationAndReentrantRestore(t *testing.T) {
	governor := newWave39ProcessGovernor(t)
	for _, limits := range []ProcessAdmissionLimits{{Active: -1}, {Active: 3}, {Queue: -1}} {
		if restore, err := ConfigureProcessAdmission(governor, limits); restore != nil || !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid configuration admitted", limits, err)
		}
	}
	if restore, err := ConfigureProcessAdmission(nil, ProcessAdmissionLimits{}); restore != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("nil process admission accepted", err)
	}
	restore, err := ConfigureProcessAdmission(governor, ProcessAdmissionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	processResources.mu.Lock()
	defaults := processResources.activeCapacity() == 2 && processResources.queueCapacity() == 8
	processResources.mu.Unlock()
	if !defaults {
		t.Fatal("zero limits changed embedded admission defaults")
	}
	remote := &wave39ProcessPermit{}
	var releaseFailure error
	remote.release = func() error {
		restore()
		processResources.mu.Lock()
		defer processResources.mu.Unlock()
		if processResources.active != 1 || processResources.bulk != 1 {
			releaseFailure = errors.New("local admission released before remote cleanup")
		}
		return releaseFailure
	}
	governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
	_, release, err := AcquireResources(context.Background(), ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := ConfigureProcessAdmission(governor, ProcessAdmissionLimits{Active: 1}); other != nil || !errors.Is(err, domain.ErrConflict) {
		t.Fatal("live process configuration replaced", err)
	}
	release()
	release()
	if releaseFailure != nil || remote.releases.Load() != 1 {
		t.Fatal("reentrant cleanup failed", releaseFailure, remote.releases.Load())
	}
	assertResourcesReleased(t, processResources)
	second := newWave39ProcessGovernor(t)
	restoreSecond, err := ConfigureProcessAdmission(second, ProcessAdmissionLimits{Active: 1, Queue: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restoreSecond()
	restore()
	processResources.mu.Lock()
	unchanged := processResources.governor == second && processResources.activeCapacity() == 1 && processResources.queueCapacity() == 1
	processResources.mu.Unlock()
	if !unchanged {
		t.Fatal("stale restore overwrote newer startup configuration")
	}
	restoreSecond()
}

func TestWave39ProcessAdmissionRestoreDefersUntilLivePermitCloses(t *testing.T) {
	governor := newWave39ProcessGovernor(t)
	restore, err := ConfigureProcessAdmission(governor, ProcessAdmissionLimits{Active: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	_, release, err := AcquireResources(context.Background(), ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	restore()
	if _, _, err := acquireResources(context.Background(), ResourceInteractive, false); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("restoring configuration admitted new work", err)
	}
	processResources.mu.Lock()
	retained := processResources.active == 1 && processResources.governor == governor
	processResources.mu.Unlock()
	if !retained {
		t.Fatal("restore discarded a live configuration")
	}
	release()
	processResources.mu.Lock()
	reset := processResources.governor == nil && processResources.configuration == nil && processResources.activeCapacity() == 2
	processResources.mu.Unlock()
	if !reset {
		t.Fatal("last release did not restore embedded admission")
	}
}

func TestWave39ProcessAdmissionWorkContextOutlivesAcquisitionBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		caller, cancelCaller := context.WithTimeout(ctx, time.Minute)
		defer cancelCaller()
		bound, release, err := AcquireResources(caller, ResourceBulk)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		permit := bound.Value(resourcePermitKey{}).(*resourcePermit)
		time.Sleep(2 * time.Second)
		deadline, hasDeadline := bound.Deadline()
		originalDeadline, _ := caller.Deadline()
		if bound.Err() != nil || !hasDeadline || !deadline.Equal(originalDeadline) || !permit.live.Load() {
			t.Fatal("acquisition timeout escaped into work lifetime", bound.Err(), deadline)
		}
		nested, done, err := AcquireResources(bound, ResourceInteractive)
		if err != nil || nested.Value(resourcePermitKey{}) != permit {
			t.Fatal("nested permit identity changed", err)
		}
		done()
		governor.cancel()
		synctest.Wait()
		if !errors.Is(bound.Err(), context.Canceled) {
			t.Fatal("control loss did not cancel live work", bound.Err())
		}
		release()
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessAdmissionSharesOneLocalAndRemoteWaitBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		first, second := &wave39ProcessPermit{}, &wave39ProcessPermit{}
		var remaining time.Duration
		governor.acquire = func(waitCtx context.Context, bulk, wait bool) (outbound.ProcessAdmissionPermit, error) {
			if governor.acquisitions.Load() == 1 {
				return first, nil
			}
			deadline, ok := waitCtx.Deadline()
			if !ok || !bulk || !wait {
				return nil, domain.ErrValidation
			}
			remaining = time.Until(deadline)
			<-waitCtx.Done()
			return second, waitCtx.Err()
		}
		_, release, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		started := time.Now()
		result := make(chan error, 1)
		go func() {
			_, done, failure := AcquireResources(ctx, ResourceBulk)
			if done != nil {
				done()
			}
			result <- failure
		}()
		synctest.Wait()
		time.Sleep(600 * time.Millisecond)
		release()
		if err := <-result; !errors.Is(err, domain.ErrResourceOverloaded) {
			t.Fatal("wrong shared wait failure", err)
		}
		if elapsed := time.Since(started); elapsed != time.Second || remaining != 400*time.Millisecond {
			t.Fatal("local and remote waits received independent budgets", elapsed, remaining)
		}
		if second.releases.Load() != 1 {
			t.Fatal("error accompanied by uncertain grant leaked remote permit")
		}
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessAdmissionCallerStillOwnsLongRunningWorkContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, _ := wave39ProcessContext(t, 2)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		bound, release, err := AcquireResources(ctx, ResourceInteractive)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		time.Sleep(2 * time.Second)
		if bound.Err() != nil {
			t.Fatal("successful grant retained acquisition timeout", bound.Err())
		}
		cancel()
		if !errors.Is(bound.Err(), context.Canceled) {
			t.Fatal("live work detached from original caller", bound.Err())
		}
		release()
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessAdmissionRemoteErrorsAndCallerCancellationReleaseLocal(t *testing.T) {
	for _, failure := range []error{domain.ErrValidation, errors.ErrUnsupported, domain.ErrConflict, domain.ErrResourceOverloaded, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, gate, governor := wave39ProcessContext(t, 2)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			remote := &wave39ProcessPermit{}
			governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) {
				if failure == context.Canceled {
					cancel()
				}
				return remote, failure
			}
			if _, done, err := AcquireResources(ctx, ResourceBulk); done != nil || !errors.Is(err, failure) {
				t.Fatal("remote failure changed", err)
			}
			if remote.releases.Load() != 1 {
				t.Fatal("partial remote grant leaked")
			}
			assertResourcesReleased(t, gate)
		})
	}
}

func TestWave39ProcessAdmissionTryAndNestedUpgrade(t *testing.T) {
	ctx, gate, governor := wave39ProcessContext(t, 2)
	remote := &wave39ProcessPermit{}
	governor.acquire = func(waitCtx context.Context, bulk, wait bool) (outbound.ProcessAdmissionPermit, error) {
		if waitCtx.Err() != nil || bulk || wait {
			return nil, domain.ErrValidation
		}
		return remote, nil
	}
	remote.upgrade = func(waitCtx context.Context, wait bool) error {
		if waitCtx.Err() != nil || wait {
			return domain.ErrValidation
		}
		return nil
	}
	bound, release, err := acquireResources(ctx, ResourceInteractive, false)
	if err != nil {
		t.Fatal("try received expired context or wrong mode", err)
	}
	defer release()
	permit := bound.Value(resourcePermitKey{}).(*resourcePermit)
	for range 2 {
		nested, done, err := acquireResources(bound, ResourceBulk, false)
		if err != nil || nested.Value(resourcePermitKey{}) != permit {
			t.Fatal("nested acquisition did not reuse outer permit", err)
		}
		done()
	}
	if governor.acquisitions.Load() != 1 || remote.upgrades.Load() != 1 || remote.releases.Load() != 0 {
		t.Fatal("nested lifetime reacquired or released remote permit")
	}
	release()
	if remote.releases.Load() != 1 {
		t.Fatal("outer response did not release remote permit")
	}
	assertResourcesReleased(t, gate)
}

func TestWave39ProcessAdmissionFailedUpgradeRollsBackBulkReservation(t *testing.T) {
	for _, canceledAfterGrant := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "canceled-grant"}[canceledAfterGrant], func(t *testing.T) {
			ctx, gate, governor := wave39ProcessContext(t, 2)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			remote := &wave39ProcessPermit{upgrade: func(context.Context, bool) error {
				if canceledAfterGrant {
					cancel()
					return nil
				}
				return domain.ErrConflict
			}}
			governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
			bound, release, err := AcquireResources(ctx, ResourceInteractive)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			want := domain.ErrConflict
			if canceledAfterGrant {
				want = context.Canceled
			}
			if _, _, err := AcquireResources(bound, ResourceBulk); !errors.Is(err, want) {
				t.Fatal("wrong upgrade failure", err)
			}
			gate.mu.Lock()
			permit := bound.Value(resourcePermitKey{}).(*resourcePermit)
			unchanged := gate.active == 1 && gate.bulk == 0 && permit.class == ResourceInteractive
			gate.mu.Unlock()
			if !unchanged || (canceledAfterGrant && remote.parks.Load() != 1) {
				t.Fatal("failed upgrade retained bulk reservation")
			}
			release()
			assertResourcesReleased(t, gate)
		})
	}
}

func TestWave39ProcessAdmissionReadinessAndSingleActiveRole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 1)
		gate.limits.Queue = 1
		governor.ready.Store(false)
		if _, _, err := AcquireResources(ctx, ResourceInteractive); !errors.Is(err, domain.ErrResourceOverloaded) || governor.acquisitions.Load() != 0 {
			t.Fatal("not-ready roster admitted work", err)
		}
		governor.ready.Store(true)
		_, release, err := AcquireResources(ctx, ResourceInteractive)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		queued, cancel := context.WithCancel(ctx)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, done, failure := AcquireResources(queued, ResourceInteractive)
			if done != nil {
				done()
			}
			result <- failure
		}()
		synctest.Wait()
		started := time.Now()
		if _, _, err := acquireResources(ctx, ResourceBulk, false); !errors.Is(err, domain.ErrResourceOverloaded) || time.Since(started) != 0 || governor.acquisitions.Load() != 1 {
			t.Fatal("single-active role bypassed local try admission", err)
		}
		if _, _, err := AcquireResources(ctx, ResourceInteractive); !errors.Is(err, domain.ErrResourceOverloaded) {
			t.Fatal("configured queue limit ignored", err)
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		release()
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessAdmissionControlLossCancelsLocalAndRemoteWaits(t *testing.T) {
	for _, remoteWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remoteWait], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, gate, governor := wave39ProcessContext(t, 2)
				var release func()
				if remoteWait {
					governor.acquire = func(waitCtx context.Context, _, _ bool) (outbound.ProcessAdmissionPermit, error) {
						<-waitCtx.Done()
						return nil, waitCtx.Err()
					}
				} else {
					var err error
					_, release, err = AcquireResources(ctx, ResourceBulk)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				result := make(chan error, 1)
				go func() {
					_, done, failure := AcquireResources(ctx, ResourceBulk)
					if done != nil {
						done()
					}
					result <- failure
				}()
				synctest.Wait()
				governor.cancel()
				if err := <-result; !errors.Is(err, domain.ErrResourceOverloaded) {
					t.Fatal("control loss did not fail closed", err)
				}
				if release != nil {
					release()
				}
				assertResourcesReleased(t, gate)
			})
		})
	}
}

func TestWave39ProcessAdmissionCombinedQueuePreservesImmediateInteractiveSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		gate.limits.Queue = 1
		_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseBulk()
		queued, cancel := context.WithCancel(ctx)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, release, failure := AcquireResources(queued, ResourceBulk)
			if release != nil {
				release()
			}
			result <- failure
		}()
		synctest.Wait()
		_, releaseRead, err := acquireResources(ctx, ResourceInteractive, false)
		if err != nil {
			t.Fatal("full bulk queue consumed immediate interactive reserve", err)
		}
		defer releaseRead()
		if _, _, err := acquireResources(ctx, ResourceInteractive, false); !errors.Is(err, domain.ErrResourceOverloaded) {
			t.Fatal("configured queue was doubled by resource class", err)
		}
		if governor.acquisitions.Load() != 2 {
			t.Fatal("queued work bypassed local active bound")
		}
		releaseRead()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		releaseBulk()
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessAdmissionPrivateWorkErrorAndPanicRelease(t *testing.T) {
	ctx, gate, governor := wave39ProcessContext(t, 2)
	remote := &wave39ProcessPermit{}
	governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
	failure := errors.New("work failed")
	if err := resourceWork(ctx, ResourceBulk, func(context.Context) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing panic")
			}
		}()
		_ = resourceWork(ctx, ResourceBulk, func(context.Context) error { panic("wave39") })
	}()
	if remote.releases.Load() != 2 {
		t.Fatal("private work lifetime leaked remote permit")
	}
	assertResourcesReleased(t, gate)
}

func TestWave39ProcessAdmissionActualDocumentResponseOwnsRemoteLifetime(t *testing.T) {
	governor := newWave39ProcessGovernor(t)
	remote := &wave39ProcessPermit{}
	governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
	restore, err := ConfigureProcessAdmission(governor, ProcessAdmissionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	config, _, repo, document := readServicesFixture(t)
	services, err := NewReadServices(config)
	if err != nil {
		t.Fatal(err)
	}
	var retained func()
	ctx := inbound.WithDocumentResponseRetainer(inbound.WithSystemActor(context.Background()), func(release func()) bool {
		retained = release
		return true
	})
	defer func() {
		if retained != nil {
			retained()
		}
	}()
	page, err := services.Documents.ReadDocFindFragment(ctx, repo, document.Hash, domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4096})
	if err != nil || page.JSON == "" || retained == nil || remote.releases.Load() != 0 || governor.acquisitions.Load() != 1 {
		t.Fatal("actual document response escaped remote lifetime", err)
	}
	retained()
	retained()
	if remote.releases.Load() != 1 {
		t.Fatal("response cleanup did not release exactly once")
	}
	assertResourcesReleased(t, processResources)
}

func TestWave39ProcessYieldRemoteParkDispatchAndResumeOrdering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		var traceMu sync.Mutex
		var trace []string
		record := func(event string) { traceMu.Lock(); trace = append(trace, event); traceMu.Unlock() }
		allowPark, enteredPark := make(chan struct{}), make(chan struct{})
		first, second := &wave39ProcessPermit{}, &wave39ProcessPermit{}
		var owner *resourcePermit
		first.park = func(context.Context) error {
			if !gate.mu.TryLock() {
				return errors.New("park held gate mutex")
			}
			valid := gate.bulk == 1 && gate.retained == nil && !owner.parked
			gate.mu.Unlock()
			if !valid {
				return errors.New("local dispatch preceded remote park")
			}
			record("park")
			close(enteredPark)
			<-allowPark
			return nil
		}
		first.resume = func(context.Context) error {
			if !gate.mu.TryLock() {
				return errors.New("resume held gate mutex")
			}
			valid := gate.bulk == 1 && !owner.parked && gate.retained == nil
			gate.mu.Unlock()
			if !valid {
				return errors.New("remote resume preceded local grant")
			}
			record("resume")
			return nil
		}
		first.release = func() error { record("release-owner"); return nil }
		second.release = func() error {
			if !gate.mu.TryLock() {
				return errors.New("release held gate mutex")
			}
			valid := gate.bulk == 1 && gate.active == 2
			gate.mu.Unlock()
			if !valid {
				return errors.New("local release preceded remote release")
			}
			record("release-other")
			return nil
		}
		governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) {
			if governor.acquisitions.Load() == 1 {
				return first, nil
			}
			record("acquire-other")
			return second, nil
		}
		bound, permit, release := ownedYieldPermit(t, ctx)
		owner = permit
		defer release()
		finishOther, startedOther := make(chan struct{}), make(chan struct{})
		otherDone := make(chan error, 1)
		go func() {
			otherDone <- resourceWork(ctx, ResourceBulk, func(context.Context) error { close(startedOther); <-finishOther; return nil })
		}()
		synctest.Wait()
		yielded := make(chan error, 1)
		go func() { yielded <- yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}) }()
		<-enteredPark
		synctest.Wait()
		if governor.acquisitions.Load() != 1 {
			t.Fatal("local waiter ran before remote park completed")
		}
		close(allowPark)
		<-startedOther
		close(finishOther)
		if err := <-otherDone; err != nil {
			t.Fatal(err)
		}
		if err := <-yielded; err != nil {
			t.Fatal(err)
		}
		release()
		traceMu.Lock()
		actual := append([]string(nil), trace...)
		traceMu.Unlock()
		if !reflect.DeepEqual(actual, []string{"park", "acquire-other", "release-other", "resume", "release-owner"}) {
			t.Fatal("remote lifecycle order changed", actual)
		}
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessYieldSingleActiveAndBorrowedReservationsDecline(t *testing.T) {
	for _, reason := range []string{"single-active", "borrowed", "keep-permit"} {
		t.Run(reason, func(t *testing.T) {
			active := 2
			if reason == "single-active" {
				active = 1
			}
			ctx, gate, governor := wave39ProcessContext(t, active)
			remote := &wave39ProcessPermit{}
			governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
			bound, _, release := ownedYieldPermit(t, ctx)
			defer release()
			if reason == "borrowed" {
				bound = context.WithValue(bound, resourceYieldOwnerKey{}, (*resourceYieldOwner)(nil))
			}
			resources := outbound.RootPublicationResources{HeapBytes: 1, KeepPermit: reason == "keep-permit"}
			gate.mu.Lock()
			gate.waiters[ResourceBulk] = []*resourceWaiter{{ready: make(chan struct{}), queued: time.Now()}}
			gate.mu.Unlock()
			if err := yieldPreparedRoot(bound, resources); err != nil {
				t.Fatal(err)
			}
			gate.mu.Lock()
			unchanged := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && len(gate.waiters[ResourceBulk]) == 1
			gate.waiters[ResourceBulk] = nil
			gate.mu.Unlock()
			if !unchanged || remote.parks.Load()+remote.resumes.Load() != 0 {
				t.Fatal("ineligible reservation parked")
			}
			release()
			assertResourcesReleased(t, gate)
		})
	}
}

func TestWave39ProcessYieldResumeFailureRetainsCleanupReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		first := &wave39ProcessPermit{resume: func(context.Context) error { return domain.ErrConflict }}
		governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) {
			if governor.acquisitions.Load() == 1 {
				return first, nil
			}
			return &wave39ProcessPermit{}, nil
		}
		bound, permit, release := ownedYieldPermit(t, ctx)
		defer release()
		otherDone := make(chan error, 1)
		go func() { otherDone <- resourceWork(ctx, ResourceBulk, func(context.Context) error { return nil }) }()
		synctest.Wait()
		if err := yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("resume error changed", err)
		}
		if err := <-otherDone; err != nil {
			t.Fatal(err)
		}
		gate.mu.Lock()
		retained := permit.parked && gate.retained == permit && gate.active == 1 && gate.bulk == 0 && len(gate.waiters[ResourceBulk]) == 0
		gate.mu.Unlock()
		if !retained || first.releases.Load() != 0 {
			t.Fatal("failed resume freed owned cleanup reservation")
		}
		release()
		assertResourcesReleased(t, gate)
	})
}

func TestWave39ProcessYieldParkFailurePreservesLiveBulkAndQueue(t *testing.T) {
	ctx, gate, governor := wave39ProcessContext(t, 2)
	remote := &wave39ProcessPermit{park: func(context.Context) error { return errors.ErrUnsupported }}
	governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) { return remote, nil }
	bound, permit, release := ownedYieldPermit(t, ctx)
	defer release()
	waiter := &resourceWaiter{ready: make(chan struct{}), queued: time.Now()}
	gate.mu.Lock()
	gate.waiters[ResourceBulk] = []*resourceWaiter{waiter}
	gate.mu.Unlock()
	if err := yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("remote park failure changed", err)
	}
	gate.mu.Lock()
	unchanged := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked && !permit.transition && permit.resume == nil &&
		len(gate.waiters[ResourceBulk]) == 1 && gate.waiters[ResourceBulk][0] == waiter && !waiter.granted
	gate.waiters[ResourceBulk] = nil
	gate.mu.Unlock()
	if !unchanged || remote.resumes.Load() != 0 {
		t.Fatal("remote park failure dispatched or lost local reservation")
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestWave39ProcessYieldCallerCancellationDoesNotResumeRemote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate, governor := wave39ProcessContext(t, 2)
		first := &wave39ProcessPermit{}
		governor.acquire = func(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) {
			if governor.acquisitions.Load() == 1 {
				return first, nil
			}
			return &wave39ProcessPermit{}, nil
		}
		bound, permit, release := ownedYieldPermit(t, ctx)
		defer release()
		bound, cancel := context.WithCancel(bound)
		defer cancel()
		startedOther, finishOther := make(chan struct{}), make(chan struct{})
		otherDone := make(chan error, 1)
		go func() {
			otherDone <- resourceWork(ctx, ResourceBulk, func(context.Context) error { close(startedOther); <-finishOther; return nil })
		}()
		synctest.Wait()
		yielded := make(chan error, 1)
		go func() { yielded <- yieldPreparedRoot(bound, outbound.RootPublicationResources{HeapBytes: 1}) }()
		<-startedOther
		cancel()
		if err := <-yielded; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		gate.mu.Lock()
		retained := gate.active == 2 && gate.bulk == 1 && gate.retained == permit && permit.parked && len(gate.waiters[ResourceBulk]) == 0
		gate.mu.Unlock()
		if !retained || first.resumes.Load() != 0 {
			t.Fatal("canceled parked owner resumed or lost cleanup reservation")
		}
		release()
		close(finishOther)
		if err := <-otherDone; err != nil {
			t.Fatal(err)
		}
		assertResourcesReleased(t, gate)
	})
}
