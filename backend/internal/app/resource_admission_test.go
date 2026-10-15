package app

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func admissionTestContext(wait time.Duration) (context.Context, *resourceAdmission) {
	gate := &resourceAdmission{wait: wait}
	return context.WithValue(context.Background(), resourceAdmissionKey{}, gate), gate
}

func waitResourceQueue(t *testing.T, gate *resourceAdmission, class ResourceClass, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		actual := len(gate.waiters[class])
		gate.mu.Unlock()
		if actual == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("admission queue did not reach expected length")
}

func assertResourcesReleased(t *testing.T, gate *resourceAdmission) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 || gate.bulk != 0 || len(gate.waiters[0])+len(gate.waiters[1]) != 0 {
		t.Fatalf("resources leaked: active=%d bulk=%d queued=%d/%d", gate.active, gate.bulk, len(gate.waiters[0]), len(gate.waiters[1]))
	}
}

func TestResourceAdmissionDispatchReservesBothClasses(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		active   int
		bulk     int
		wantBulk bool
		wantRead bool
	}{
		{name: "idle serves both queues", wantBulk: true, wantRead: true},
		{name: "read backlog cannot starve bulk", active: 1, wantBulk: true},
		{name: "bulk preserves read capacity", active: 1, bulk: 1, wantRead: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			bulk := &resourceWaiter{ready: make(chan struct{})}
			read := &resourceWaiter{ready: make(chan struct{})}
			followingRead := &resourceWaiter{ready: make(chan struct{})}
			gate := &resourceAdmission{active: scenario.active, bulk: scenario.bulk}
			gate.waiters[ResourceBulk] = []*resourceWaiter{bulk}
			gate.waiters[ResourceInteractive] = []*resourceWaiter{read, followingRead}
			gate.dispatch()
			if bulk.granted != scenario.wantBulk || read.granted != scenario.wantRead || followingRead.granted {
				t.Fatalf("dispatch grants bulk=%v read=%v followingRead=%v", bulk.granted, read.granted, followingRead.granted)
			}
			if gate.active != 2 || gate.bulk != 1 {
				t.Fatalf("admission counts active=%d bulk=%d", gate.active, gate.bulk)
			}
		})
	}
}

func TestResourceAdmissionReserveQueueCancellation(t *testing.T) {
	ctx, gate := admissionTestContext(time.Minute)
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	queued, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, resourceQueueLimit)
	for range resourceQueueLimit {
		go func() {
			_, release, err := AcquireResources(queued, ResourceBulk)
			if release != nil {
				release()
			}
			results <- err
		}()
	}
	waitResourceQueue(t, gate, ResourceBulk, resourceQueueLimit)
	if _, _, err := AcquireResources(ctx, ResourceBulk); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("full queue: %v", err)
	}
	_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatalf("bulk work consumed interactive reserve: %v", err)
	}
	releaseRead()
	cancel()
	for range resourceQueueLimit {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation: %v", err)
		}
	}
	releaseBulk()
	assertResourcesReleased(t, gate)
}

func TestResourceAdmissionNestedPermitReleaseAndExpiry(t *testing.T) {
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, class := range []ResourceClass{ResourceBulk, ResourceInteractive} {
		_, nestedRelease, err := AcquireResources(bound, class)
		if err != nil {
			t.Fatal(err)
		}
		nestedRelease()
	}
	if _, _, err := AcquireResources(ctx, ResourceBulk); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("nested release returned outer permit: %v", err)
	}
	release()
	release()
	_, otherRelease, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := AcquireResources(bound, ResourceBulk); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("stale context bypassed admission: %v", err)
	}
	otherRelease()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := AcquireResources(canceled, ResourceBulk); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertResourcesReleased(t, gate)
}

func TestResourceAdmissionCancellationReleaseRace(t *testing.T) {
	ctx, gate := admissionTestContext(time.Minute)
	for range 100 {
		_, release, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			t.Fatal(err)
		}
		queued, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, release, err := AcquireResources(queued, ResourceBulk)
			if release != nil {
				release()
			}
			done <- err
		}()
		waitResourceQueue(t, gate, ResourceBulk, 1)
		go cancel()
		release()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		cancel()
		assertResourcesReleased(t, gate)
	}
}

func TestResourceAdmissionNestedUpgradeDoesNotBypassBulkLimit(t *testing.T) {
	ctx, gate := admissionTestContext(time.Minute)
	interactive, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead()
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	if _, _, err := AcquireResources(interactive, ResourceBulk); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("interactive context bypassed occupied bulk slot: %v", err)
	}
	releaseBulk()
	_, releaseNested, err := AcquireResources(interactive, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	releaseNested()
	gate.mu.Lock()
	active, bulk := gate.active, gate.bulk
	gate.mu.Unlock()
	if active != 1 || bulk != 1 {
		t.Fatalf("upgrade not charged to outer lifetime: active=%d bulk=%d", active, bulk)
	}
	releaseRead()
	assertResourcesReleased(t, gate)
}

func TestResourceAdmissionBeforeJobClaimAndFailureRelease(t *testing.T) {
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	claimed := 0
	claim := func(context.Context) (int, error) { claimed++; return 1, nil }
	failure := errors.New("job failed")
	run := func(bound context.Context, _ int) error {
		_, nestedRelease, err := AcquireResources(bound, ResourceBulk)
		if err != nil {
			return err
		}
		nestedRelease()
		return failure
	}
	if err := admittedJob(ctx, claim, run); !errors.Is(err, domain.ErrResourceOverloaded) || claimed != 0 {
		t.Fatalf("leased work while waiting: claimed=%d err=%v", claimed, err)
	}
	release()
	if err := admittedJob(ctx, claim, run); !errors.Is(err, failure) || claimed != 1 {
		t.Fatalf("job error changed: %v", err)
	}
	assertResourcesReleased(t, gate)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing panic")
			}
		}()
		_ = resourceWork(ctx, ResourceBulk, func(context.Context) error { panic("worker panic") })
	}()
	assertResourcesReleased(t, gate)
}

func TestResourceAdmissionMixedBuffersBounded(t *testing.T) {
	ctx, gate := admissionTestContext(time.Minute)
	var active, bulk, peak atomic.Int64
	var workers sync.WaitGroup
	errorsOut := make(chan error, 48)
	for index := range 48 {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			class := ResourceBulk
			if index%3 == 0 {
				class = ResourceInteractive
			}
			err := resourceWork(ctx, class, func(bound context.Context) error {
				if active.Add(1) > 2 {
					errorsOut <- errors.New("more than two active operations")
				}
				defer active.Add(-1)
				if class == ResourceBulk {
					if bulk.Add(1) > 1 {
						errorsOut <- errors.New("more than one active bulk operation")
					}
					defer bulk.Add(-1)
				}
				buffer := make([]byte, 4<<20)
				for offset := 0; offset < len(buffer); offset += 4096 {
					buffer[offset] = byte(index)
				}
				live := active.Load() * int64(len(buffer))
				for previous := peak.Load(); live > previous && !peak.CompareAndSwap(previous, live); previous = peak.Load() {
				}
				_, nested, err := AcquireResources(bound, class)
				if err != nil {
					return err
				}
				nested()
				runtime.KeepAlive(buffer)
				return nil
			})
			if err != nil && !errors.Is(err, domain.ErrResourceOverloaded) {
				errorsOut <- err
			}
		}(index)
	}
	workers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	if peak.Load() == 0 || peak.Load() > 8<<20 {
		t.Fatalf("synthetic live buffer bound: %d", peak.Load())
	}
	assertResourcesReleased(t, gate)
	t.Logf("synthetic admitted buffers peak <= %d bytes; not process RSS", peak.Load())
}

func TestResourceAdmissionWholeDocumentReadsShareBulkBeforeStorage(t *testing.T) {
	ctx, gate := admissionTestContext(5 * time.Millisecond)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	service := &Service{}
	repo, hash := hh("read repo"), hh("read doc")
	reads := map[string]func() error{
		"events":             func() error { _, err := service.ReadDocEvents(ctx, repo, hash, "", 0, 1); return err },
		"fragments":          func() error { _, err := service.ReadDocFragments(ctx, repo, hash, 0, 0, 1, 1024); return err },
		"search events":      func() error { _, err := service.SearchDocEvents(ctx, repo, hash, "query", -1, 1); return err },
		"matching documents": func() error { _, err := service.MatchingDocHashes(ctx, repo, "query"); return err },
		"agent history": func() error {
			_, err := service.ReadAgentHistoryPage(ctx, repo, hash, domain.AgentHistoryPageRequest{Before: -1, Limit: 1, MaxBytes: 4096})
			return err
		},
		"raw document":  func() error { _, err := service.GetDoc(ctx, repo, hash); return err },
		"fetch objects": func() error { _, err := service.Send(ctx, inbound.PullSendInput{RepoID: repo}); return err },
		"pull chunks": func() error {
			_, err := service.PullChunks(ctx, inbound.PullChunksInput{RepoID: repo, Wants: []domain.ContentHash{hash}})
			return err
		},
		"search": func() error {
			_, err := service.Search(ctx, inbound.SearchInput{RepoID: repo, Query: "query"})
			return err
		},
		"diff": func() error {
			_, err := service.Diff(ctx, inbound.DiffInput{RepoID: repo, Left: hash, Right: hash})
			return err
		},
		"fsck": func() error { _, err := service.Fsck(ctx, repo); return err },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, domain.ErrResourceOverloaded) {
				t.Fatalf("whole-document path escaped shared bulk gate: %v", err)
			}
		})
	}
	release()
	assertResourcesReleased(t, gate)
}
