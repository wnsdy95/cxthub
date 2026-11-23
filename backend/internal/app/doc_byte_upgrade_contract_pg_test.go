//go:build postgres

package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docByteUpgradeContractPGStore struct {
	*legacyFindGenerationPGStore
	metadataDemands int
	byteDemands     int
}

func (storage *docByteUpgradeContractPGStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.metadataDemands++
	return storage.PostgresStore.EnqueueDocRangeJob(ctx, repo, hash, now)
}

func (storage *docByteUpgradeContractPGStore) EnqueueDocByteRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.byteDemands++
	return storage.PostgresStore.EnqueueDocByteRangeJob(ctx, repo, hash, now)
}
