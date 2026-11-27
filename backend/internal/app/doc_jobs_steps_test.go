package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docJobLease = documentworker.Lease

// processDocFinalizations runs waiting finalization jobs the way the program
// does: through the document worker built over the service's stores.
func processDocFinalizations(ctx context.Context, service *Service, limit int) error {
	workers, err := service.DocumentWorkersForProfile(documentquery.LegacyCompatibility)
	if err != nil {
		return err
	}
	return workers.Finalization.Process(ctx, limit)
}

// The steps of that worker, one at a time, against a job store the test
// chooses. The program reaches them through Process.
func testDocumentFinalizer(service *Service, storage outbound.DocJobStore) (*documentworker.Finalizer, error) {
	scope := newDocumentWorkerScope(service.meta, service.blobs, documentquery.LegacyCompatibility)
	return documentworker.NewFinalizer(storage, service.blobs, &service.docVerifier, scope)
}

func verifyDocJob(ctx context.Context, service *Service, job domain.DocFinalizationJob) (domain.VerifiedSessionDoc, error) {
	finalizer, err := testDocumentFinalizer(service, nil)
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	return finalizer.Verify(ctx, job)
}

func runDocJob(ctx context.Context, service *Service, storage outbound.DocJobStore, job domain.DocFinalizationJob) error {
	finalizer, err := testDocumentFinalizer(service, storage)
	if err != nil {
		return err
	}
	return finalizer.RunJob(ctx, job)
}

func yieldRootPreparation(ctx context.Context, service *Service, publication outbound.PreparedDocPublication) error {
	finalizer, err := testDocumentFinalizer(service, nil)
	if err != nil {
		return err
	}
	return finalizer.YieldRootPreparation(ctx, publication)
}
