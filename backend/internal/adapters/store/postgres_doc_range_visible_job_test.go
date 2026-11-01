//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocRangeVisibleJobPGUpgradesCompletedRangeOnlyReceipt(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
	prior := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
	var version int64
	if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if ready, err := storage.HasDocVisibleIndex(ctx, repo, fixture.hash); err != nil || ready {
		t.Fatal("missing visible index is ready", ready, err)
	}
	if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
		t.Fatal("visible miss removed legacy range readiness", ready, err)
	}
	if receipt := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash); !reflect.DeepEqual(receipt, prior) {
		t.Fatal("legacy range request unnecessarily requeued", receipt, prior)
	}
	queued, err := storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, time.Now().UTC())
	if err != nil || queued.State != "waiting" || queued.Version != prior.Version+1 || queued.BaseHash != "" {
		t.Fatal("range-only receipt did not upgrade", queued, err)
	}
	claim = docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if ready, err := storage.HasDocVisibleIndex(ctx, repo, fixture.hash); err != nil || !ready {
		t.Fatal("upgrade did not publish visible generation", ready, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		visible, err := storage.OpenDocVisibleGeneration(read, repo, fixture.hash)
		if err != nil {
			return err
		}
		_, _, err = visible.Unit(read, "chat", 128, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&after); err != nil || after != version {
		t.Fatal("projection upgrade changed source", version, after, err)
	}
	completed := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
	if receipt, err := storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(receipt, completed) {
		t.Fatal("ready visible receipt changed", receipt, err)
	}
}

func TestDocRangeVisibleJobPGSharesRangeJobIdentity(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 2)
	type result struct {
		job domain.DocRangeIndexJob
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 12)
	now := time.Now().UTC().Add(-time.Hour)
	for index := range 12 {
		go func() {
			<-start
			var receipt result
			if index%2 == 0 {
				receipt.job, receipt.err = storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, now)
			} else {
				receipt.job, receipt.err = storage.EnqueueDocRangeJob(ctx, repo, fixture.hash, now)
			}
			results <- receipt
		}()
	}
	close(start)
	for range 12 {
		receipt := <-results
		if receipt.err != nil || receipt.job.State != "waiting" || receipt.job.Version != 0 || receipt.job.BaseHash != "" {
			t.Fatal("mixed request receipt", receipt)
		}
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 1 {
		t.Fatal("visible requests forked queue identity", count, err)
	}
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if ready, err := storage.HasDocVisibleIndex(ctx, repo, fixture.hash); err != nil || !ready {
		t.Fatal("shared worker omitted visible index", ready, err)
	}
}

func TestDocRangeVisibleJobPGRejectsForeignScopeAndCorruptCurrentIndex(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	foreign, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 2)
	if _, err := storage.EnqueueDocVisibleIndex(ctx, foreign, fixture.hash, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign visible job accepted", err)
	}
	if ready, err := storage.HasDocVisibleIndex(ctx, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) || ready {
		t.Fatal("foreign presence exposed", ready, err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_visible_generations_v1 SET descriptor=$2 WHERE doc_hash=$1`, fixture.hash, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if ready, err := storage.HasDocVisibleIndex(ctx, repo, fixture.hash); err != nil || !ready {
		t.Fatal("presence probe performed integrity validation", ready, err)
	}
	if _, err := storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, claim); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("worker silently repaired current corrupt index", err)
	}
	if job := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); job.State != "running" || job.Version != claim.Version {
		t.Fatal("failed publication reported completion", job)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		_, err := storage.OpenDocVisibleGeneration(read, repo, fixture.hash)
		return err
	}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corruption was hidden after worker failure", err)
	}
}

func TestDocRangeVisibleJobPGUpgradeEnqueuePreservesBudget(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 2)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
	prior := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	blocker, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-enqueue:"+string(repo)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("visible upgrade ignored enqueue budget", err)
	}
	if after := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); !reflect.DeepEqual(after, prior) {
		t.Fatal("timed out upgrade changed receipt", after, prior)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := storage.EnqueueDocVisibleIndex(ctx, repo, fixture.hash, time.Now().UTC()); err != nil || job.State != "waiting" {
		t.Fatal("upgrade could not resume", job, err)
	}
}

func TestDocRangeVisibleJobPGLegacyMetadataUpgradeRebindsPrefix(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 2, 2)
	pair[0], pair[1] = pair[1], pair[0]
	hash := pair[0].hash
	docRangeJobPGEnqueue(t, storage, ctx, repo, hash)
	if err := storage.BuildDocRangeJob(ctx, docRangeJobPGClaim(t, storage, ctx, repo, hash)); err != nil {
		t.Fatal(err)
	}
	header, err := storage.DocRangeIndex(ctx, repo, hash)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := storage.DocRangePage(ctx, repo, header, 0)
	if err != nil {
		t.Fatal(err)
	}
	for position := range entries {
		entries[position].RenderedBefore = 0
		entries[position].SeedSummary = false
		entries[position].EmptyReasoning = false
	}
	page, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	pageHash := domain.HashContent(page)
	directory, err := json.Marshal(map[string]any{"hashes": []domain.ContentHash{pageHash}, "level": 0, "start": 0})
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := storage.readRangeGeneration(ctx, repo, hash)
	if err != nil {
		t.Fatal(err)
	}
	ranges.EventPages.Hash = domain.HashContent(directory)
	descriptor, err := ranges.Encode()
	if err != nil {
		t.Fatal(err)
	}
	legacyHash := domain.HashContent(descriptor)
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(transaction)
	for _, object := range []domain.DocRangeObject{{Hash: pageHash, Bytes: page}, {Hash: ranges.EventPages.Hash, Bytes: directory}} {
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, hash, object.Hash, object.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := transaction.Exec(ctx, `UPDATE doc_range_generations_v2 SET generation_hash=$2,descriptor=$3 WHERE doc_hash=$1`, hash, legacyHash, descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM doc_visible_generations_v1 WHERE doc_hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
	if err := storage.BuildDocRangeJob(ctx, docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)); err != nil {
		t.Fatal(err)
	}
	docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 2, nil)
	prior := docRangeBoundaryPGGet(t, storage, ctx, repo, pair)
	var sourceVersion int64
	if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, hash).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	if job, err := storage.EnqueueDocVisibleIndex(ctx, repo, hash, time.Now().UTC()); err != nil || job.State != "waiting" {
		t.Fatal("legacy presentation upgrade", job, err)
	}
	if err := storage.BuildDocRangeJob(ctx, docRangeJobPGClaim(t, storage, ctx, repo, hash)); err != nil {
		t.Fatal(err)
	}
	var currentHash domain.ContentHash
	var currentVersion int64
	if err := storage.pool.QueryRow(ctx, `SELECT source_version,generation_hash FROM doc_range_generations_v2 WHERE doc_hash=$1`, hash).Scan(&currentVersion, &currentHash); err != nil || currentHash == legacyHash || currentVersion != sourceVersion {
		t.Fatal("upgrade changed source or retained legacy generation", currentVersion, sourceVersion, currentHash, err)
	}
	docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
	queued := docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
	if queued.State != "waiting" || queued.Version != prior.Version+1 {
		t.Fatal("prefix proof reused old presentation generation", queued, prior)
	}
	if err := storage.BuildDocRangeJob(ctx, docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)); err != nil {
		t.Fatal(err)
	}
	docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 2, nil)
}
