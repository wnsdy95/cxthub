//go:build postgres

package http

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentPreparationRevocationStore struct {
	*store.PostgresStore
	before    func() error
	triggered atomic.Bool
	enqueues  atomic.Int32
}

func (storage *documentPreparationRevocationStore) readiness(ready bool, err error) (bool, error) {
	if err == nil && !ready && !storage.triggered.Swap(true) {
		err = storage.before()
	}
	return ready, err
}

func (storage *documentPreparationRevocationStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	return storage.readiness(ready, err)
}

func (storage *documentPreparationRevocationStore) HasDocByteRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocByteRangeIndex(ctx, repo, hash)
	return storage.readiness(ready, err)
}

func (storage *documentPreparationRevocationStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.enqueues.Add(1)
	return storage.PostgresStore.EnqueueDocRangeJob(ctx, repo, hash, now)
}

func (storage *documentPreparationRevocationStore) EnqueueDocByteRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.enqueues.Add(1)
	return storage.PostgresStore.EnqueueDocByteRangeJob(ctx, repo, hash, now)
}
