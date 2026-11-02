package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pendingProjectionAdmissionStore struct {
	outbound.MetadataStore
	outbound.PendingProjectionStore
	outbound.RepositoryViewSourceStore
	claim func(context.Context, time.Duration) (domain.PendingProjectionLease, error)
}

func (storage *pendingProjectionAdmissionStore) ClaimPendingProjection(ctx context.Context, duration time.Duration) (domain.PendingProjectionLease, error) {
	return storage.claim(ctx, duration)
}

func TestPendingProjectionClaimDeadlineReleasesAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		parentTimeout time.Duration
		queued        time.Duration
		claimBudget   time.Duration
	}{
		{name: "background parent", claimBudget: 30 * time.Second},
		{name: "earlier parent deadline", parentTimeout: 5 * time.Second, claimBudget: 5 * time.Second},
		{name: "budget starts after admission", queued: 20 * time.Second, claimBudget: 30 * time.Second},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, gate := admissionTestContext(time.Minute)
				parent := ctx
				if scenario.parentTimeout != 0 {
					var cancel context.CancelFunc
					parent, cancel = context.WithTimeout(ctx, scenario.parentTimeout)
					defer cancel()
				}
				type observation struct {
					ctx      context.Context
					deadline time.Time
					started  time.Time
					duration time.Duration
					bounded  bool
				}
				entered := make(chan observation, 1)
				storage := &pendingProjectionAdmissionStore{claim: func(claim context.Context, duration time.Duration) (domain.PendingProjectionLease, error) {
					deadline, bounded := claim.Deadline()
					entered <- observation{ctx: claim, deadline: deadline, started: time.Now(), duration: duration, bounded: bounded}
					if !bounded || time.Until(deadline) > 30*time.Second {
						return domain.PendingProjectionLease{}, errors.New("claim has no bounded deadline")
					}
					<-claim.Done()
					return domain.PendingProjectionLease{}, claim.Err()
				}}
				service := &Service{meta: storage}
				var release func()
				if scenario.queued != 0 {
					var err error
					_, release, err = AcquireResources(ctx, ResourceBulk)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				started := time.Now()
				done := make(chan error, 1)
				go func() { done <- service.ProcessPendingProjections(parent, 1) }()
				if scenario.queued != 0 {
					synctest.Wait()
					gate.mu.Lock()
					queued := len(gate.waiters[ResourceBulk])
					gate.mu.Unlock()
					if queued != 1 || len(entered) != 0 {
						t.Fatalf("claim ran before admission: queued=%d claims=%d", queued, len(entered))
					}
					time.Sleep(scenario.queued)
					release()
				}
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("claim timeout was swallowed or changed: %v", err)
				}
				got := <-entered
				if !got.bounded || got.deadline.Sub(got.started) != scenario.claimBudget || got.duration != time.Minute {
					t.Fatalf("claim deadline or lease duration: bounded=%v budget=%v lease=%v", got.bounded, got.deadline.Sub(got.started), got.duration)
				}
				if elapsed := time.Since(started); elapsed != scenario.queued+scenario.claimBudget {
					t.Fatalf("claim timeout includes admission wait: elapsed=%v queued=%v budget=%v", elapsed, scenario.queued, scenario.claimBudget)
				}
				if !errors.Is(got.ctx.Err(), context.DeadlineExceeded) || scenario.parentTimeout == 0 && parent.Err() != nil {
					t.Fatalf("claim cancellation escaped its scope: claim=%v parent=%v", got.ctx.Err(), parent.Err())
				}
				assertResourcesReleased(t, gate)
				if err := resourceWork(ctx, ResourceBulk, func(context.Context) error { return nil }); err != nil {
					t.Fatalf("timed-out claim retained bulk admission: %v", err)
				}
				assertResourcesReleased(t, gate)
			})
		})
	}
}
