//go:build postgres

package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayCatalogReuseProbePG struct {
	*store.PostgresStore
	saveCalls     int
	completeCalls int
}

func (probe *docDisplayCatalogReuseProbePG) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	probe.saveCalls++
	return probe.PostgresStore.SaveDocEventDisplay(ctx, job, step)
}

func (probe *docDisplayCatalogReuseProbePG) CompleteDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	probe.completeCalls++
	return probe.PostgresStore.CompleteDocEventDisplay(ctx, job, checkpoint, now)
}
