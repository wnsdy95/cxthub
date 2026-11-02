package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type yieldingPreparationMetadata struct {
	streamingPreparationMetadata
	nested bool
}

func (metadata yieldingPreparationMetadata) InTransaction(context.Context) bool {
	return metadata.nested
}

type yieldingRootPublication struct {
	resources outbound.RootPublicationResources
	complete  func(context.Context) error
	close     func() error
}

func (publication *yieldingRootPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	return publication.resources, nil
}

func (publication *yieldingRootPublication) Complete(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time) error {
	return publication.complete(ctx)
}

func (publication *yieldingRootPublication) Close() error {
	return publication.close()
}

func TestDocJobRootPreparationYieldsOnlyOwnedBoundedReservation(t *testing.T) {
	for _, reason := range []string{"owned", "borrowed", "transaction", "unknown-transaction", "unknown-resources", "oversized", "cleanup-error"} {
		t.Run(reason, func(t *testing.T) {
			ctx, gate := admissionTestContext(time.Second)
			metadata := yieldingPreparationMetadata{nested: reason == "transaction"}
			service := NewService(metadata, nil, nil, nil, nil)
			if reason == "unknown-transaction" {
				service.meta = streamingPreparationMetadata{}
			}
			representation, _ := rootWorkerFixture(t)
			job, err := domain.NewDocFinalizationJobForRepresentation(hh(t.Name()), representation, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			job = job.Claim(time.Now().UTC(), docJobLease)
			var uploaded atomic.Bool
			completed, closed := 0, 0
			cleanupError := errors.New("remove prepared spool failed")
			shouldYield := reason == "owned" || reason == "cleanup-error"
			publication := &yieldingRootPublication{resources: outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}, complete: func(bound context.Context) error {
				completed++
				if uploaded.Load() != shouldYield {
					t.Error("wrong phase ordering: upload before completion", uploaded.Load())
				}
				gate.mu.Lock()
				valid := gate.bulk == 1 && gate.active == 1 && gate.retained == nil
				gate.mu.Unlock()
				if !valid {
					t.Error("completion ran without exclusive bulk admission")
				}
				return nil
			}, close: func() error {
				closed++
				gate.mu.Lock()
				reserved := gate.active == 1
				gate.mu.Unlock()
				if !reserved {
					t.Error("active reservation released before prepared cleanup")
				}
				if reason == "cleanup-error" {
					return cleanupError
				}
				return nil
			}}
			if reason == "oversized" {
				publication.resources.HeapBytes = maxParkedRootHeapBytes + 1
			}
			uploadDone := make(chan error, 1)
			storage := streamingPreparationStore{preparationJobStore: &preparationJobStore{}, prepareRoot: func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
				go func() {
					_, release, err := AcquireResources(ctx, ResourceBulk)
					if err == nil {
						uploaded.Store(true)
						release()
					}
					uploadDone <- err
				}()
				waitResourceQueue(t, gate, ResourceBulk, 1)
				if reason == "unknown-resources" {
					return struct {
						outbound.PreparedRootDocPublication
					}{publication}, nil
				}
				return publication, nil
			}}
			jobCtx := ctx
			releaseParent := func() {}
			if reason == "borrowed" {
				jobCtx, releaseParent, err = AcquireResources(ctx, ResourceBulk)
				if err != nil {
					t.Fatal(err)
				}
				defer releaseParent()
			}
			err = admittedJob(jobCtx, func(context.Context) (domain.DocFinalizationJob, error) { return job, nil }, func(bound context.Context, job domain.DocFinalizationJob) error {
				return service.runDocJob(bound, storage, job)
			})
			if reason == "cleanup-error" {
				if !errors.Is(err, cleanupError) {
					t.Fatal("cleanup failure was hidden", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			releaseParent()
			if err := <-uploadDone; err != nil {
				t.Fatal(err)
			}
			if completed != 1 || closed != 1 {
				t.Fatal("completion or cleanup repeated", completed, closed)
			}
			assertResourcesReleased(t, gate)
		})
	}
}

func TestDocJobRootLeaseLossWhileParkedPreventsCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, gate := admissionTestContext(time.Minute)
		service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
		representation, _ := rootWorkerFixture(t)
		job, err := domain.NewDocFinalizationJobForRepresentation(hh(t.Name()), representation, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		job = job.Claim(time.Now().UTC(), docJobLease)
		var renewals, finishes, completions, closes atomic.Int32
		publication := &yieldingRootPublication{resources: outbound.RootPublicationResources{HeapBytes: 4096}, complete: func(context.Context) error {
			completions.Add(1)
			return nil
		}, close: func() error {
			closes.Add(1)
			gate.mu.Lock()
			reserved := gate.active == 2 && gate.retained != nil
			gate.mu.Unlock()
			if !reserved {
				t.Error("cleanup lost retained admission")
			}
			return nil
		}}
		prepared, continuePrepare := make(chan struct{}), make(chan struct{})
		storage := streamingPreparationStore{preparationJobStore: &preparationJobStore{renew: func(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error {
			renewals.Add(1)
			return domain.ErrConflict
		}, finish: func(context.Context, domain.DocFinalizationJob, time.Time) error {
			finishes.Add(1)
			return nil
		}}, prepareRoot: func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
			close(prepared)
			<-continuePrepare
			return publication, nil
		}}
		result := make(chan error, 1)
		go func() {
			result <- admittedJob(ctx, func(context.Context) (domain.DocFinalizationJob, error) { return job, nil }, func(bound context.Context, job domain.DocFinalizationJob) error {
				return service.runDocJob(bound, storage, job)
			})
		}()
		<-prepared
		finishUpload, uploadDone := make(chan struct{}), make(chan error, 1)
		go func() {
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err == nil {
				<-finishUpload
				release()
			}
			uploadDone <- err
		}()
		synctest.Wait()
		close(continuePrepare)
		synctest.Wait()
		time.Sleep(docJobLease / 3)
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal("lost renewal allowed completion", err)
		}
		close(finishUpload)
		if err := <-uploadDone; err != nil {
			t.Fatal(err)
		}
		if renewals.Load() != 1 || finishes.Load() != 1 || completions.Load() != 0 || closes.Load() != 1 {
			t.Fatal("unexpected parked lease lifecycle", renewals.Load(), finishes.Load(), completions.Load(), closes.Load())
		}
		assertResourcesReleased(t, gate)
	})
}
