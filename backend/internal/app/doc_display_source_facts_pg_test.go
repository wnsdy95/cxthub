//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplaySnapshotFactsPG struct {
	*store.PostgresStore
	reads   int
	failure error
}

func (storage *docDisplaySnapshotFactsPG) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.reads++
	if storage.failure != nil {
		return domain.Snapshot{}, storage.failure
	}
	return storage.PostgresStore.GetSnapshot(ctx, repo, hash)
}
