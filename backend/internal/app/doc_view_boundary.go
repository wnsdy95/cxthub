package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var errDocViewBoundaryNeedsPreparation = fmt.Errorf("%w: inherited prefix is not ready", domain.ErrDocRangeIndexUnavailable)

func (service *Service) prepareDocViewBoundary(ctx context.Context, repo, hash, base domain.ContentHash) error {
	storage, ok := service.blobs.(outbound.DocRangeBoundaryStore)
	if !ok {
		return domain.ErrDocRangeIndexUnavailable
	}
	if outbound.DocReadOnly(ctx) {
		return domain.ErrDocRangeIndexUnavailable
	}
	return resourceWork(ctx, ResourceInteractive, func(admitted context.Context) error {
		admitted, err := reauthorizeDocumentRead(admitted, repo)
		if err != nil {
			return err
		}
		job, err := storage.EnqueueDocRangeBoundary(admitted, repo, hash, base, time.Now().UTC())
		if err != nil {
			return err
		}
		return docRangePreparationError(job)
	})
}

func (service *Service) docViewBoundary(ctx context.Context, repo, hash, base domain.ContentHash, source, previous docViewSource) (int, error) {
	storage, ok := service.blobs.(outbound.DocRangeBoundaryStore)
	durable := source.root && previous.root || source.binding.Validate() == nil && previous.binding.Validate() == nil
	if !ok || !durable {
		return docViewInherited(ctx, source, previous)
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return 0, domain.ErrConflict
	}
	inherited, err := storage.DocRangeBoundary(ctx, repo, hash, base)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return 0, errDocViewBoundaryNeedsPreparation
	}
	if err != nil {
		return 0, err
	}
	if inherited < 0 || inherited > min(source.total, previous.total) {
		return 0, domain.ErrIntegrity
	}
	return inherited, nil
}
