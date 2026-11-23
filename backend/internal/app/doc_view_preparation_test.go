package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyViewPreparationStore struct {
	*legacyFindGenerationStore
	outbound.DocRangeJobStore
	ready          bool
	readinessError error
	checks         int
	queued         int
	enqueue        func(context.Context) (domain.DocRangeIndexJob, error)
}

func (storage *legacyViewPreparationStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	storage.checks++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if repo != storage.repo || storage.fixtures[hash] == nil {
		return false, domain.ErrNotFound
	}
	return storage.ready, storage.readinessError
}

func (storage *legacyViewPreparationStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.queued++
	if repo != storage.repo || storage.fixtures[hash] == nil || now.IsZero() {
		return domain.DocRangeIndexJob{}, domain.ErrIntegrity
	}
	return storage.enqueue(ctx)
}
