//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docByteRangeReadySQL = `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 generation
 WHERE generation.doc_hash=owned.hash AND generation.source_version=source.doc_range_source_version
 AND (generation.range_layout_version>=4 OR generation.range_stream_bytes=0))
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`

const docByteRangeJobReceiptSQL = `SELECT ` + docRangeJobColumns + ` FROM doc_range_jobs_v1 job
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash='' AND job.minimum_range_version>=4
 AND EXISTS(SELECT 1 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash
 WHERE snapshot.repo_id=job.repo_id AND snapshot.id=job.doc_hash AND snapshot.doc_hash=job.doc_hash
 AND snapshot.doc_identity='' AND source.doc_range_source_version=job.byte_source_version)`

func (storage *PostgresStore) LegacyByteRangePublicationEnabled() bool {
	return storage.legacyByteRangePublication
}

func (storage *PostgresStore) HasDocByteRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return false, err
	}
	var ready bool
	if err := storage.db(ctx).QueryRow(ctx, docByteRangeReadySQL, repo, hash).Scan(&ready); err != nil {
		return false, mapNoRows(err)
	}
	if ready {
		return true, ctx.Err()
	}
	generation, err := storage.readRangeGeneration(ctx, repo, hash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if generation.Identity() != domain.DocumentIdentityLegacy {
		return false, domain.ErrUnsupportedDocumentIdentity
	}
	return generation.Version >= 4 || generation.StreamBytes == 0, ctx.Err()
}

func (storage *PostgresStore) EnqueueDocByteRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	if !storage.legacyByteRangePublication {
		return domain.DocRangeIndexJob{}, domain.ErrDocRangeIndexUnavailable
	}
	return storage.enqueueDocRangeJobMinimum(ctx, repo, hash, "", now, false, 4)
}

func (storage *PostgresStore) docByteRangeJobReceipt(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndexJob, error) {
	job, err := scanDocRangeJob(storage.db(ctx).QueryRow(ctx, docByteRangeJobReceiptSQL, repo, hash))
	if err == nil && job.State == "rejected" && job.Reason == "source_unavailable" {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	if err != nil || job.State != "completed" {
		return job, err
	}
	ready, err := storage.HasDocByteRangeIndex(ctx, repo, hash)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if !ready {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	return job, nil
}

var _ outbound.DocByteRangePreparationStore = (*PostgresStore)(nil)
