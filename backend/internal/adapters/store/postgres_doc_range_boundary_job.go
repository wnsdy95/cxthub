//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docRangeBoundaryLiveSQL = `SELECT EXISTS(SELECT 1 FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 AND state='running' AND version=$4 AND lease_until>clock_timestamp()
 AND EXISTS(SELECT 1 FROM repository_catalog_state catalog WHERE catalog.repo_id=$1 AND catalog.epoch::text=$5))`

func lockDocRangeBoundaryScope(ctx context.Context, transaction pgx.Tx, repo, doc, base domain.ContentHash) error {
	hashes := docRangeBoundaryHashes(doc, base)
	rows, err := transaction.Query(ctx, `SELECT snapshot.id FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=ANY($2::text[]) AND snapshot.doc_hash=snapshot.id AND snapshot.doc_identity IN ($3,$4)
 ORDER BY snapshot.id FOR SHARE OF snapshot,owned`, repo, hashes, domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(hashes) {
		return domain.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) EnqueueDocRangeBoundary(ctx context.Context, repo, doc, base domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	if err := domain.ValidateContentHash(base); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return s.enqueueDocRangeJob(ctx, repo, doc, base, now, false)
}

func (s *PostgresStore) buildDocRangeBoundaryJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, borrowed := ctx.Value(repositoryTxKey{}).(*repositoryTx); borrowed || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	var sources [2]docRangeBoundarySource
	var checkpoint docRangeBoundaryCheckpoint
	var exists bool
	var progress domain.DocRangePrefixProgress
	err := s.WithinReadSnapshot(ctx, func(read context.Context) error {
		var live bool
		if err := s.db(read).QueryRow(read, docRangeBoundaryLiveSQL, job.RepoID, job.DocHash, job.BaseHash, job.Version, job.RepositoryEpoch).Scan(&live); err != nil {
			return err
		}
		if !live {
			return domain.ErrConflict
		}
		var err error
		sources, err = s.readDocRangeBoundarySources(read, job.RepoID, job.DocHash, job.BaseHash)
		if err != nil {
			return err
		}
		checkpoint, exists, err = readDocRangeBoundaryCheckpoint(read, s.db(read), job.RepoID, job.DocHash, job.BaseHash, false)
		if err != nil {
			return err
		}
		start := 0
		if exists {
			if checkpoint.jobVersion >= job.Version {
				return domain.ErrConflict
			}
			if checkpoint.matches(sources) {
				if err := checkpoint.validate(sources); err != nil {
					return err
				}
				if checkpoint.complete {
					progress = domain.DocRangePrefixProgress{Events: checkpoint.events, Complete: true}
					return nil
				}
				start = checkpoint.events
			}
		}
		if job.DocHash == job.BaseHash {
			progress = domain.DocRangePrefixProgress{Events: int(sources[0].generation.EventCount), Complete: true}
			return nil
		}
		var inputs [2]domain.DocRangePrefixSource
		for position, source := range sources {
			reader := &postgresDocRangeGenerationRead{
				transaction: read.Value(repositoryTxKey{}).(*repositoryTx),
				generation:  source.generation,
				objects:     make(map[domain.ContentHash][]byte),
			}
			inputs[position] = domain.DocRangePrefixSource{Total: int(source.generation.EventCount), PageHash: reader.PageHash, ReadPage: reader.ReadPage}
		}
		progress, err = domain.CompareDocRangePrefix(read, inputs[0], inputs[1], start, domain.DocRangePrefixBatchPages)
		return err
	})
	if err != nil {
		return err
	}
	return s.publishDocRangeBoundary(ctx, job, sources, checkpoint, exists, progress)
}

func lockDocRangeBoundaryRoots(ctx context.Context, transaction pgx.Tx, hashes []domain.ContentHash) error {
	for _, statement := range []string{
		`SELECT hash FROM blobs WHERE hash=ANY($1::text[]) ORDER BY hash FOR UPDATE`,
		`SELECT doc_hash FROM doc_range_generations_v2 WHERE doc_hash=ANY($1::text[]) ORDER BY doc_hash FOR SHARE`,
	} {
		rows, err := transaction.Query(ctx, statement, hashes)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if count != len(hashes) {
			return domain.ErrConflict
		}
	}
	return nil
}

func (s *PostgresStore) publishDocRangeBoundary(ctx context.Context, job domain.DocRangeIndexJob, sources [2]docRangeBoundarySource, checkpoint docRangeBoundaryCheckpoint, exists bool, progress domain.DocRangePrefixProgress) error {
	transaction, err := s.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocRangeBoundaryRoots(ctx, transaction, docRangeBoundaryHashes(job.DocHash, job.BaseHash)); err != nil {
		return err
	}
	if err := lockDocRangeBoundaryScope(ctx, transaction, job.RepoID, job.DocHash, job.BaseHash); err != nil {
		return err
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: s, repo: job.RepoID})
	current, err := s.readDocRangeBoundarySources(bound, job.RepoID, job.DocHash, job.BaseHash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	for position, source := range sources {
		if !source.sameGeneration(current[position]) {
			return domain.ErrConflict
		}
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	prior, priorExists, err := readDocRangeBoundaryCheckpoint(bound, s.db(bound), job.RepoID, job.DocHash, job.BaseHash, true)
	if err != nil {
		return err
	}
	if priorExists != exists || prior != checkpoint {
		return domain.ErrConflict
	}
	next := docRangeBoundaryCheckpoint{
		docVersion: sources[0].version, baseVersion: sources[1].version,
		docDescriptor: sources[0].descriptor, baseDescriptor: sources[1].descriptor,
		jobVersion: job.Version, events: progress.Events, complete: progress.Complete,
	}
	if err := next.validate(sources); err != nil {
		return err
	}
	if exists && checkpoint.matches(sources) && (next.events < checkpoint.events ||
		checkpoint.complete && (!next.complete || next.events != checkpoint.events)) {
		return domain.ErrConflict
	}
	state := "waiting"
	if progress.Complete {
		state = "completed"
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state=$5,reason='',lease_until=NULL,next_attempt=clock_timestamp(),
 attempts=CASE WHEN $5='waiting' THEN 0 ELSE attempts END
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 AND version=$4 AND state='running' AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.BaseHash, job.Version, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	_, err = transaction.Exec(ctx, `INSERT INTO doc_range_boundaries_v1
 (repo_id,doc_hash,base_hash,algorithm,doc_source_version,base_source_version,doc_generation_hash,base_generation_hash,job_version,events,complete)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
 ON CONFLICT(repo_id,doc_hash,base_hash,algorithm) DO UPDATE SET doc_source_version=EXCLUDED.doc_source_version,
 base_source_version=EXCLUDED.base_source_version,doc_generation_hash=EXCLUDED.doc_generation_hash,
 base_generation_hash=EXCLUDED.base_generation_hash,job_version=EXCLUDED.job_version,events=EXCLUDED.events,complete=EXCLUDED.complete`,
		job.RepoID, job.DocHash, job.BaseHash, domain.DocRangePrefixAlgorithm, next.docVersion, next.baseVersion,
		next.docDescriptor, next.baseDescriptor, next.jobVersion, next.events, next.complete)
	if err != nil {
		return err
	}
	return transaction.Commit(ctx)
}
