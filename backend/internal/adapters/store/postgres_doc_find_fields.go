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

const docFindFieldsLiveSQL = `SELECT EXISTS(SELECT 1 FROM doc_find_fields_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND version=$3 AND state='running'
 AND source_version=$4 AND lease_until>clock_timestamp())`

const docFindFieldsActiveCountSQL = `SELECT count(*) FROM (
 SELECT 1 FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND state IN ('waiting','running','retrying') LIMIT $2
) active`

func (s *PostgresStore) ReadDocFindFields(ctx context.Context, repo, doc, event domain.ContentHash, eventBytes int) (domain.DocFindFields, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocFindFields{}, err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return domain.DocFindFields{}, domain.ErrConflict
	}
	if err := validateHashes(repo, doc, event); err != nil {
		return domain.DocFindFields{}, err
	}
	if eventBytes <= 0 {
		return domain.DocFindFields{}, domain.ErrValidation
	}
	var version, storedEventBytes int
	var projectionHash domain.ContentHash
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT fields.version,fields.event_bytes,
 substring(fields.projection_hash FROM 1 FOR $5),substring(fields.bytes FROM 1 FOR $4)
 FROM repo_blobs owned JOIN doc_find_fields_v1 fields ON fields.event_hash=$3
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`,
		repo, doc, event, domain.MaxDocFindFieldsBytes+1, len(event)+1).Scan(&version, &storedEventBytes, &projectionHash, &raw)
	if err != nil {
		return domain.DocFindFields{}, mapNoRows(err)
	}
	if version != 1 || storedEventBytes != eventBytes {
		return domain.DocFindFields{}, domain.ErrIntegrity
	}
	fields, err := domain.DecodeDocFindFields(projectionHash, event, eventBytes, raw)
	if err != nil {
		return domain.DocFindFields{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.DocFindFields{}, err
	}
	return fields, nil
}

func lockDocFindFieldsSource(ctx context.Context, transaction pgx.Tx, repo, doc domain.ContentHash) (int64, error) {
	var sourceVersion int64
	if err := transaction.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1 FOR SHARE`, doc).Scan(&sourceVersion); err != nil {
		return 0, mapNoRows(err)
	}
	var present int
	if err := transaction.QueryRow(ctx, docRangeJobSourceSQL+` FOR SHARE OF snapshot,owned`, repo, doc, domain.DocumentIdentityRootV1).Scan(&present); err != nil {
		return 0, mapNoRows(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT 1 FROM doc_range_generations_v2
 WHERE doc_hash=$1 AND source_version=$2 FOR SHARE`, doc, sourceVersion).Scan(&present); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrDocRangeIndexUnavailable
		}
		return 0, err
	}
	return sourceVersion, nil
}

func (s *PostgresStore) EnqueueDocFindFields(ctx context.Context, repo, doc domain.ContentHash, now time.Time) (queued domain.DocRangeIndexJob, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = docRangeEnqueueError(parent, failure) }()
	if err := validateHashes(repo, doc); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if now.IsZero() {
		return domain.DocRangeIndexJob{}, domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-find-fields-enqueue:"+string(repo)); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	sourceVersion, err := lockDocFindFieldsSource(ctx, transaction, repo, doc)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	prior, err := scanDocRangeJob(transaction.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_find_fields_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND source_version=$3 FOR UPDATE`, repo, doc, sourceVersion))
	if err == nil && prior.State != "completed" {
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		return prior, nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.DocRangeIndexJob{}, err
	}
	if errors.Is(err, domain.ErrNotFound) {
		prior, err = scanDocRangeJob(transaction.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_find_fields_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 FOR UPDATE`, repo, doc))
	}
	version := int64(0)
	replacesActive := false
	if err == nil {
		version = prior.Version + 1
		replacesActive = prior.State == "waiting" || prior.State == "running" || prior.State == "retrying"
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DocRangeIndexJob{}, err
	}
	var pending int
	if err := transaction.QueryRow(ctx, docFindFieldsActiveCountSQL, repo, domain.MaxPendingDocRangeJobs).Scan(&pending); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if pending >= domain.MaxPendingDocRangeJobs && !replacesActive {
		return domain.DocRangeIndexJob{}, domain.ErrResourceOverloaded
	}
	job, err := scanDocRangeJob(transaction.QueryRow(ctx, `INSERT INTO doc_find_fields_jobs_v1
 (repo_id,doc_hash,source_version,state,version,created_at,next_attempt) VALUES($1,$2,$3,'waiting',$4,$5,$5)
 ON CONFLICT(repo_id,doc_hash) DO UPDATE SET source_version=EXCLUDED.source_version,state='waiting',reason='',attempts=0,
 version=EXCLUDED.version,created_at=EXCLUDED.created_at,next_attempt=EXCLUDED.next_attempt,lease_until=NULL
 RETURNING `+docRangeJobColumns, repo, doc, sourceVersion, version, now))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return job, nil
}

func (s *PostgresStore) ClaimDocFindFields(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	if now.IsZero() || lease <= 0 {
		return domain.DocRangeIndexJob{}, domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	defer rollbackPG(transaction)
	job, err := scanDocRangeJob(transaction.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_find_fields_jobs_v1 job
 WHERE job.state IN ('waiting','retrying','running') AND job.next_attempt<=$1
 AND (job.state<>'running' OR (job.lease_until<=$1 AND job.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM doc_find_fields_jobs_v1 other
 WHERE other.repo_id=job.repo_id AND other.doc_hash<>job.doc_hash AND other.state='running')
 ORDER BY job.next_attempt,job.created_at,job.repo_id,job.doc_hash FOR UPDATE OF job SKIP LOCKED LIMIT 1`, now))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-find-fields-claim:"+string(job.RepoID)).Scan(&locked); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if !locked {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	var other bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_find_fields_jobs_v1
 WHERE repo_id=$1 AND doc_hash<>$2 AND state='running')`, job.RepoID, job.DocHash).Scan(&other); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if other {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	job, err = scanDocRangeJob(transaction.QueryRow(ctx, `UPDATE doc_find_fields_jobs_v1
 SET state='running',reason='',attempts=attempts+1,version=version+1,lease_until=$3
 WHERE repo_id=$1 AND doc_hash=$2 AND $3>clock_timestamp() RETURNING `+docRangeJobColumns,
		job.RepoID, job.DocHash, now.Add(lease)))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return job, nil
}

func validateDocFindFieldsJob(job domain.DocRangeIndexJob) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if job.BaseHash != "" {
		return domain.ErrValidation
	}
	return nil
}

func lockDocFindFieldsJob(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob) error {
	var present int
	err := transaction.QueryRow(ctx, `SELECT 1 FROM doc_find_fields_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 FOR UPDATE`, job.RepoID, job.DocHash).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrConflict
	}
	return err
}

func lockDocFindFieldsJobSource(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob) (int64, error) {
	sourceVersion, err := lockDocFindFieldsSource(ctx, transaction, job.RepoID, job.DocHash)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return 0, domain.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return sourceVersion, nil
}

func (s *PostgresStore) RenewDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
	if err := validateDocFindFieldsJob(job); err != nil {
		return err
	}
	if job.State != "running" || now.IsZero() || lease <= 0 {
		return domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocFindFieldsJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_find_fields_jobs_v1 SET lease_until=GREATEST(lease_until,$5)
 WHERE repo_id=$1 AND doc_hash=$2 AND version=$3 AND state='running' AND lease_until>$4 AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Version, now, now.Add(lease))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) PutDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, fields domain.DocFindFields) error {
	if err := validateDocFindFieldsJob(job); err != nil {
		return err
	}
	if job.State != "running" {
		return domain.ErrValidation
	}
	raw, err := fields.Encode()
	if err != nil {
		return err
	}
	projectionHash := domain.HashContent(raw)
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	sourceVersion, err := lockDocFindFieldsJobSource(ctx, transaction, job)
	if err != nil {
		return err
	}
	if err := lockDocFindFieldsJob(ctx, transaction, job); err != nil {
		return err
	}
	checkLive := func() error {
		var live bool
		if err := transaction.QueryRow(ctx, docFindFieldsLiveSQL, job.RepoID, job.DocHash, job.Version, sourceVersion).Scan(&live); err != nil {
			return err
		}
		if !live {
			return domain.ErrConflict
		}
		return nil
	}
	if err := checkLive(); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `INSERT INTO doc_find_fields_v1(version,event_hash,event_bytes,projection_hash,bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(event_hash) DO NOTHING`, fields.Version, fields.EventHash, fields.EventBytes, projectionHash, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var agrees bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_find_fields_v1
 WHERE event_hash=$1 AND version=$2 AND event_bytes=$3 AND projection_hash=$4 AND bytes=$5)`,
			fields.EventHash, fields.Version, fields.EventBytes, projectionHash, raw).Scan(&agrees); err != nil {
			return err
		}
		if !agrees {
			return domain.ErrIntegrity
		}
	}
	if err := checkLive(); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) FinishDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	if err := validateDocFindFieldsJob(job); err != nil {
		return err
	}
	if (job.State != "waiting" && job.State != "retrying" && job.State != "rejected" && job.State != "completed") || now.IsZero() || len(job.Reason) > 128 {
		return domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	sourceCheck := ""
	arguments := []any{job.RepoID, job.DocHash, job.Version, now, job.State, job.Reason, job.NextAttempt}
	if job.State == "completed" {
		sourceVersion, err := lockDocFindFieldsJobSource(ctx, transaction, job)
		if err != nil {
			return err
		}
		sourceCheck = " AND source_version=$8"
		arguments = append(arguments, sourceVersion)
	}
	if err := lockDocFindFieldsJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_find_fields_jobs_v1 SET state=$5,reason=$6,next_attempt=$7,lease_until=NULL
 WHERE repo_id=$1 AND doc_hash=$2 AND version=$3 AND state='running'
 AND lease_until>$4 AND lease_until>clock_timestamp()`+sourceCheck, arguments...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

var _ outbound.DocFindFieldsStore = (*PostgresStore)(nil)
