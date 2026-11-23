//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type boundaryReceiptRaceStore struct {
	*store.PostgresStore
	fixture              docViewPGFixture
	test                 *testing.T
	mode                 string
	base                 domain.ContentHash
	calls                int
	invalidated, rebuilt bool
}

func (storage *boundaryReceiptRaceStore) EnqueueDocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.calls++
	storage.base = base
	job, err := storage.PostgresStore.EnqueueDocRangeBoundary(ctx, repo, hash, base, now)
	if err != nil || storage.calls != 1 {
		return job, err
	}
	claim, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), docJobLease)
	if err != nil || claim.DocHash != hash || claim.BaseHash != base {
		storage.test.Fatal("race fixture claimed wrong work", claim, err)
	}
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		return job, err
	}
	job, err = storage.PostgresStore.EnqueueDocRangeBoundary(ctx, repo, hash, base, now)
	if err != nil || job.State != "completed" {
		storage.test.Fatal("race fixture did not complete", job, err)
	}
	if storage.mode == "late-generation" {
		return job, err
	}
	if storage.mode == "generation" {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, base)
	} else {
		_, err = storage.fixture.pool.Exec(ctx, `DELETE FROM doc_range_boundaries_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, hash, base)
	}
	return job, err
}

func (storage *boundaryReceiptRaceStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	if err == nil && ready && storage.mode == "late-generation" && storage.calls == 1 && hash == storage.base && !storage.invalidated {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
		storage.invalidated = true
	}
	return ready, err
}

func (storage *boundaryReceiptRaceStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) && storage.mode == "late-generation" && hash == storage.base && !storage.rebuilt {
		if _, repairErr := storage.PostgresStore.EnqueueDocRangeJob(storage.fixture.ctx, repo, hash, time.Now().UTC()); repairErr != nil {
			storage.test.Fatal("race fixture enqueue failed", repairErr)
		}
		claim, repairErr := storage.PostgresStore.ClaimDocRangeJob(storage.fixture.ctx, time.Now().UTC(), docJobLease)
		if repairErr != nil || claim.DocHash != hash || claim.BaseHash != "" {
			storage.test.Fatal("race fixture claimed wrong generation", claim, repairErr)
		}
		if repairErr := storage.PostgresStore.BuildDocRangeJob(storage.fixture.ctx, claim); repairErr != nil {
			storage.test.Fatal("race fixture rebuild failed", repairErr)
		}
		storage.rebuilt = true
	}
	return read, err
}

type generationReadinessRaceStore struct {
	*store.PostgresStore
	fixture docViewPGFixture
	calls   int
}

func (storage *generationReadinessRaceStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	storage.calls++
	if err == nil && ready && storage.calls == 1 {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
	}
	return ready, err
}
