package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// One user streams multi-second bulk batches back to back; another user's
// single bulk request (a history read or a small push) arrives meanwhile.
// It returns how long the second user waited and its admission error.
func twoUserBulkScenario(t *testing.T, gate *resourceAdmission) (time.Duration, error) {
	t.Helper()
	ctx := context.WithValue(context.Background(), resourceAdmissionKey{}, gate)
	stop := make(chan struct{})
	uploader := make(chan struct{})
	go func() {
		defer close(uploader)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			time.Sleep(2 * time.Second) // one 4MB batch on a small instance
			release()                   // the next batch is requested immediately
		}
	}()
	time.Sleep(500 * time.Millisecond) // the uploader holds the bulk slot
	started := time.Now()
	_, release, err := AcquireResources(ctx, ResourceBulk)
	waited := time.Since(started)
	if err == nil {
		release()
	}
	close(stop)
	<-uploader
	return waited, err
}

func TestBulkAdmissionLetsWaitingUserInterleaveWithLongUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &resourceAdmission{wait: resourceWaitLimit, limits: ProcessAdmissionLimits{BulkWait: ResourceBulkWaitLimit}}
		waited, err := twoUserBulkScenario(t, gate)
		if err != nil || waited > 2*time.Second {
			t.Fatalf("second user starved behind a long upload: err=%v waited=%s", err, waited)
		}
		assertResourcesReleased(t, gate)
	})
}

// The unchanged one-second wait cannot outlast one batch: this documents
// the starvation the bulk wait limit removes.
func TestBulkAdmissionShortWaitStarvesBehindLongUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &resourceAdmission{wait: resourceWaitLimit}
		if _, err := twoUserBulkScenario(t, gate); !errors.Is(err, domain.ErrResourceOverloaded) {
			t.Fatalf("legacy admission changed: %v", err)
		}
	})
}

func TestBulkAdmissionLongWaitKeepsInteractiveQueueAndBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &resourceAdmission{wait: resourceWaitLimit, limits: ProcessAdmissionLimits{BulkWait: ResourceBulkWaitLimit}}
		ctx := context.WithValue(context.Background(), resourceAdmissionKey{}, gate)
		_, releaseHolder, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 8)
		share := resourceQueueLimit / 4
		for range share {
			go func() {
				_, release, err := AcquireResources(ctx, ResourceBulk)
				if err == nil {
					release()
				}
				results <- err
			}()
		}
		synctest.Wait()
		// Bulk waiters beyond a quarter of the queue are refused at once.
		started := time.Now()
		if _, _, err := AcquireResources(ctx, ResourceBulk); !errors.Is(err, domain.ErrResourceOverloaded) || time.Since(started) != 0 {
			t.Fatalf("bulk waiters exceeded their queue share: %v", err)
		}
		// Interactive requests keep their own capacity meanwhile.
		_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
		if err != nil {
			t.Fatalf("interactive request blocked by bulk waiters: %v", err)
		}
		releaseRead()
		// Waiters still give up within the bounded bulk wait.
		started = time.Now()
		for range share {
			if err := <-results; !errors.Is(err, domain.ErrResourceOverloaded) {
				t.Fatalf("bulk waiter outlived its bound: %v", err)
			}
		}
		if elapsed := time.Since(started); elapsed > ResourceBulkWaitLimit {
			t.Fatalf("bulk wait exceeded its limit: %s", elapsed)
		}
		releaseHolder()
		assertResourcesReleased(t, gate)
	})
}

func TestUnconfiguredGateKeepsOrdinaryBulkWait(t *testing.T) {
	if processResources.waitLimit(ResourceBulk) != resourceWaitLimit || processResources.waitLimit(ResourceInteractive) != resourceWaitLimit {
		t.Fatal("unconfigured process admission wait changed")
	}
	gate := &resourceAdmission{wait: resourceWaitLimit, limits: ProcessAdmissionLimits{BulkWait: ResourceBulkWaitLimit}}
	if gate.waitLimit(ResourceInteractive) != resourceWaitLimit || gate.waitLimit(ResourceBulk) != ResourceBulkWaitLimit {
		t.Fatal("configured bulk wait leaked into interactive admission")
	}
}
