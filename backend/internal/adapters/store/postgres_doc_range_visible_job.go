//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docVisibleReadySQL = `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 generation
 JOIN doc_visible_generations_v1 visible ON visible.doc_hash=generation.doc_hash
 AND visible.source_version=generation.source_version AND visible.range_generation_hash=generation.generation_hash
 WHERE generation.doc_hash=owned.hash AND generation.source_version=root.doc_range_source_version)
 FROM repo_blobs owned JOIN blobs root ON root.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`

const docVisibleJobReceiptSQL = `SELECT ` + docRangeJobColumns + ` FROM doc_range_jobs_v1 job
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash='' AND EXISTS(` + docRangeJobSourceSQL + `)
 AND (job.state<>'completed' OR EXISTS(SELECT 1 FROM doc_range_generations_v2 generation
 JOIN blobs root ON root.hash=generation.doc_hash AND generation.source_version=root.doc_range_source_version
 JOIN doc_visible_generations_v1 visible ON visible.doc_hash=generation.doc_hash
 AND visible.source_version=generation.source_version AND visible.range_generation_hash=generation.generation_hash
 WHERE generation.doc_hash=job.doc_hash))`

func (s *PostgresStore) HasDocVisibleIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return false, err
	}
	var ready bool
	if err := s.db(ctx).QueryRow(ctx, docVisibleReadySQL, repo, hash).Scan(&ready); err != nil {
		return false, mapNoRows(err)
	}
	return ready, ctx.Err()
}

func (s *PostgresStore) EnqueueDocVisibleIndex(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	return s.enqueueDocRangeJob(ctx, repo, hash, "", now, true)
}

var _ outbound.DocVisiblePreparationStore = (*PostgresStore)(nil)
