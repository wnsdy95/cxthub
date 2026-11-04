package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphProjectionWorkerDeadlinesStartAfterAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		stage   string
		queue   time.Duration
		parent  time.Duration
		budget  time.Duration
		failure bool
	}{
		{name: "claim", stage: "claim", budget: 30 * time.Second},
		{name: "queued claim", stage: "claim", queue: 20 * time.Second, budget: 30 * time.Second},
		{name: "work", stage: "load", budget: 30 * time.Second, failure: true},
		{name: "parent claim", stage: "claim", parent: 5 * time.Second, budget: 5 * time.Second},
		{name: "parent work", stage: "load", parent: 5 * time.Second, budget: 5 * time.Second},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, storage, _, _ := graphProjectionWorkerFixture(t)
				ctx, gate := admissionTestContext(time.Minute)
				if scenario.parent != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, scenario.parent)
					defer cancel()
				}
				var observed time.Duration
				storage.step = func(bound context.Context, step string) error {
					if step == scenario.stage {
						deadline, bounded := bound.Deadline()
						if !bounded {
							return errors.New("missing operation deadline")
						}
						observed = time.Until(deadline)
						<-bound.Done()
						return bound.Err()
					}
					if step == "fail" {
						deadline, bounded := bound.Deadline()
						if !bounded || time.Until(deadline) != 5*time.Second || bound.Err() != nil {
							return errors.New("failure inherited expired operation deadline")
						}
					}
					return nil
				}
				var release func()
				if scenario.queue != 0 {
					var err error
					_, release, err = AcquireResources(ctx, ResourceBulk)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				start := time.Now()
				done := make(chan error, 1)
				go func() { done <- service.ProcessGraphProjections(ctx, 1) }()
				if scenario.queue != 0 {
					synctest.Wait()
					if storage.calls["claim"] != 0 {
						t.Fatal("job claimed before bulk admission")
					}
					time.Sleep(scenario.queue)
					release()
				}
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) || observed != scenario.budget || time.Since(start) != scenario.queue+scenario.budget {
					t.Fatal("claim or work deadline changed", err, observed, time.Since(start))
				}
				if (len(storage.failures) == 1) != scenario.failure || scenario.failure && storage.failures[0].reason != "temporary_failure" {
					t.Fatal("shutdown and operation timeout were conflated", storage.failures)
				}
				assertResourcesReleased(t, gate)
			})
		})
	}
}

func TestGraphProjectionWorkerSchedulingAndDisabledDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, storage, job, _ := graphProjectionWorkerFixture(t)
		ctx, gate := admissionTestContext(time.Minute)
		service.RunGraphProjectionWorker(ctx)
		if storage.calls["claim"] != 0 {
			t.Fatal("worker ran without explicit configuration")
		}
		if err := service.ConfigureGraphProjections(ctx, []domain.ContentHash{job.RepoID}); err != nil {
			t.Fatal(err)
		}
		storage.jobs = nil
		worker, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		go func() { service.RunGraphProjectionWorker(worker); close(done) }()
		synctest.Wait()
		if storage.calls["claim"] != 1 {
			t.Fatal("worker did not poll immediately", storage.calls)
		}
		assertResourcesReleased(t, gate)
		time.Sleep(time.Second)
		synctest.Wait()
		if storage.calls["claim"] != 2 {
			t.Fatal("worker did not wait between empty polls", storage.calls)
		}
		cancel()
		<-done
		assertResourcesReleased(t, gate)
		if storage.calls["claim"] != 2 {
			t.Fatal("worker claimed after shutdown", storage.calls)
		}
	})
}
