//go:build postgres

package http

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Observes the real publication boundary without delaying it, changing leases,
// bypassing the worker or substituting persistence. This is not lock-hold time.
type publicationObservedStore struct {
	*store.PostgresStore
	publicationStart, publicationEnd time.Time
}

type publicationObserved struct {
	outbound.PreparedDocPublication
	owner *publicationObservedStore
}

type streamingPublicationObserved struct {
	outbound.PreparedRootDocPublication
	owner *publicationObservedStore
}

func (s *publicationObservedStore) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	p, err := s.PostgresStore.PrepareDocJob(ctx, doc)
	return publicationObserved{p, s}, err
}

func (s *publicationObservedStore) PrepareLegacyDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	prepared, err := s.PostgresStore.PrepareLegacyDocJob(ctx, job)
	if err != nil {
		return nil, err
	}
	return streamingPublicationObserved{prepared, s}, nil
}

func (p streamingPublicationObserved) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	staged, ok := p.PreparedRootDocPublication.(outbound.StagedDocPublication)
	if !ok {
		return fmt.Errorf("streaming publication does not support staging")
	}
	return staged.Stage(ctx, job, lease)
}

func (p streamingPublicationObserved) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	p.owner.publicationStart = time.Now()
	err := p.PreparedRootDocPublication.Complete(ctx, job, now)
	p.owner.publicationEnd = time.Now()
	return err
}

func (p publicationObserved) Complete(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	p.owner.publicationStart = time.Now()
	err := p.PreparedDocPublication.Complete(ctx, j, now)
	p.owner.publicationEnd = time.Now()
	return err
}
