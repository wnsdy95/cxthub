package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type deadlineDocJobStore struct {
	outbound.BlobStore
	outbound.DocJobStore
	claim func(context.Context) (domain.DocFinalizationJob, error)
}

func (storage deadlineDocJobStore) ClaimDocJob(ctx context.Context, _ domain.ContentHash, _ time.Time, _ time.Duration) (domain.DocFinalizationJob, error) {
	return storage.claim(ctx)
}

func TestDocFinalizationClaimHasIndependentDeadline(t *testing.T) {
	service, storage := newFsckSvc(t)
	ctx, gate := admissionTestContext(resourceWaitLimit)
	var claimContext context.Context
	service.blobs = deadlineDocJobStore{BlobStore: storage, DocJobStore: storage, claim: func(bound context.Context) (domain.DocFinalizationJob, error) {
		claimContext = bound
		deadline, bounded := bound.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
			t.Error("document claim lacks an independent five-second deadline")
		}
		return domain.DocFinalizationJob{}, domain.ErrNotFound
	}}
	if err := service.ProcessDocFinalizations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if claimContext == nil || !errors.Is(claimContext.Err(), context.Canceled) {
		t.Fatal("claim context not canceled after claim returned")
	}
	assertResourcesReleased(t, gate)
}

func TestDocFinalizationBlockedClaimRespectsCallerDeadline(t *testing.T) {
	service, storage := newFsckSvc(t)
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	service.blobs = deadlineDocJobStore{BlobStore: storage, DocJobStore: storage, claim: func(bound context.Context) (domain.DocFinalizationJob, error) {
		<-bound.Done()
		return domain.DocFinalizationJob{}, bound.Err()
	}}
	if err := service.ProcessDocFinalizations(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked claim: %v", err)
	}
	assertResourcesReleased(t, gate)
}
