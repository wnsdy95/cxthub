//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docRangeJobColumns = `repo_id,doc_hash,base_hash,state,reason,attempts,version,created_at,next_attempt,lease_until`

const docRangeEnqueueTimeout = 500 * time.Millisecond

const legacyRangeValidationRevision = 1

var errDocRangeJobReleased = errors.New("document range job lease was released")

const docRangeJobActiveCountSQL = `SELECT count(*) FROM (
 SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state IN ('waiting','running','retrying') LIMIT $2
) active`

const docRangeReadySQL = `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 generation
 WHERE generation.doc_hash=owned.hash AND generation.source_version=root.doc_range_source_version)
 FROM repo_blobs owned JOIN blobs root ON root.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`

const docRangeJobOwnedSourceSQL = `SELECT 1 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2`

const docRangeJobSourceSQL = docRangeJobOwnedSourceSQL + ` AND snapshot.doc_identity=$3`

const docRangeJobTypedSourceSQL = docRangeJobOwnedSourceSQL + ` AND snapshot.doc_identity IN ($3,$4)`

const docRangeJobIdentitySQL = `SELECT snapshot.doc_identity FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity IN ($3,$4)`

const docRangeJobReceiptSQL = `SELECT ` + docRangeJobColumns + ` FROM doc_range_jobs_v1 job
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash='' AND EXISTS(` + docRangeJobTypedSourceSQL + `)`

func (s *PostgresStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return false, err
	}
	var ready bool
	if err := s.db(ctx).QueryRow(ctx, docRangeReadySQL, repo, hash).Scan(&ready); err != nil {
		return false, mapNoRows(err)
	}
	return ready, ctx.Err()
}

func (storage *PostgresStore) docRangeJobReceipt(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndexJob, error) {
	job, err := scanDocRangeJob(storage.db(ctx).QueryRow(ctx, docRangeJobReceiptSQL, repo, hash, domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy))
	if err != nil || job.State != "completed" {
		return job, err
	}
	ready, err := storage.HasDocRangeIndex(ctx, repo, hash)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if !ready {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	return job, nil
}

func (storage *PostgresStore) retryLegacyRangeRejection(ctx context.Context, job domain.DocRangeIndexJob) (bool, error) {
	if job.State != "rejected" || job.Reason != "invalid_document" {
		return false, nil
	}
	var retry bool
	err := storage.db(ctx).QueryRow(ctx, `SELECT job.legacy_validation_revision<=$5 AND
 (job.legacy_validation_revision<$5 OR (job.minimum_range_version=4 AND EXISTS(
 SELECT 1 FROM blobs source WHERE source.hash=job.doc_hash AND source.doc_range_source_version<>job.byte_source_version))) AND EXISTS(
 SELECT 1 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 WHERE snapshot.repo_id=job.repo_id AND snapshot.id IN (job.doc_hash,job.base_hash)
 AND snapshot.doc_hash=snapshot.id AND snapshot.doc_identity='')
 FROM doc_range_jobs_v1 job
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash=$3 AND job.version=$4`,
		job.RepoID, job.DocHash, job.BaseHash, job.Version, legacyRangeValidationRevision).Scan(&retry)
	return retry, mapNoRows(err)
}

func (s *PostgresStore) beginDocRangeJob(ctx context.Context) (pgx.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, borrowed := ctx.Value(repositoryTxKey{}).(*repositoryTx); borrowed || outbound.DocReadOnly(ctx) {
		return nil, domain.ErrConflict
	}
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

func scanDocRangeJob(row pgx.Row) (domain.DocRangeIndexJob, error) {
	var job domain.DocRangeIndexJob
	var lease *time.Time
	if err := row.Scan(&job.RepoID, &job.DocHash, &job.BaseHash, &job.State, &job.Reason, &job.Attempts, &job.Version, &job.CreatedAt, &job.NextAttempt, &lease); err != nil {
		return domain.DocRangeIndexJob{}, mapNoRows(err)
	}
	if lease != nil {
		job.LeaseUntil = *lease
	}
	if err := job.Validate(); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return job, nil
}

func (s *PostgresStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	return s.enqueueDocRangeJob(ctx, repo, hash, "", now, false)
}

func docRangeEnqueueError(parent context.Context, failure error) error {
	if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		if parent.Err() != nil {
			return parent.Err()
		}
		if errors.Is(failure, context.DeadlineExceeded) {
			return domain.ErrResourceOverloaded
		}
	}
	var postgresError *pgconn.PgError
	if errors.As(failure, &postgresError) && postgresError.Code == "55P03" {
		return domain.ErrResourceOverloaded
	}
	return failure
}

func (s *PostgresStore) enqueueDocRangeJob(ctx context.Context, repo, hash, base domain.ContentHash, now time.Time, visible bool) (queued domain.DocRangeIndexJob, failure error) {
	return s.enqueueDocRangeJobMinimum(ctx, repo, hash, base, now, visible, 0)
}

func (s *PostgresStore) enqueueDocRangeJobMinimum(ctx context.Context, repo, hash, base domain.ContentHash, now time.Time, visible bool, minimum int) (queued domain.DocRangeIndexJob, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = docRangeEnqueueError(parent, failure) }()
	if err := validateHashes(repo, hash); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if base != "" {
		if err := domain.ValidateContentHash(base); err != nil {
			return domain.DocRangeIndexJob{}, err
		}
	}
	if now.IsZero() || (minimum != 0 && minimum != 4) || minimum == 4 && (visible || base != "") {
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
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: s, repo: repo})
	currentReceipt := func() (domain.DocRangeIndexJob, error) {
		if minimum == 4 {
			return s.docByteRangeJobReceipt(bound, repo, hash)
		}
		if base != "" {
			return s.docRangeBoundaryReceipt(bound, repo, hash, base)
		}
		if visible {
			return scanDocRangeJob(transaction.QueryRow(ctx, docVisibleJobReceiptSQL, repo, hash, domain.DocumentIdentityRootV1))
		}
		return s.docRangeJobReceipt(bound, repo, hash)
	}
	receipt := func() (domain.DocRangeIndexJob, error) {
		job, err := currentReceipt()
		if err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		retry, err := s.retryLegacyRangeRejection(bound, job)
		if err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		if retry {
			return domain.DocRangeIndexJob{}, domain.ErrNotFound
		}
		return job, nil
	}
	prior, err := receipt()
	if errors.Is(err, domain.ErrNotFound) {
		if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-enqueue:"+string(repo)); err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		prior, err = receipt()
	}
	if err == nil {
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		return prior, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.DocRangeIndexJob{}, err
	}
	if _, err := retainDocJobRepositoryPG(ctx, transaction, repo); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if base != "" {
		if err := lockDocRangeBoundaryScope(ctx, transaction, repo, hash, base); err != nil {
			return domain.DocRangeIndexJob{}, err
		}
	} else {
		statement := docRangeJobTypedSourceSQL
		arguments := []any{repo, hash, domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy}
		if visible {
			statement = docRangeJobSourceSQL
			arguments = arguments[:3]
		} else if minimum == 4 {
			statement = docRangeJobSourceSQL
			arguments = []any{repo, hash, domain.DocumentIdentityLegacy}
		}
		var present int
		if err := transaction.QueryRow(ctx, statement+` FOR SHARE OF snapshot,owned`, arguments...).Scan(&present); err != nil {
			return domain.DocRangeIndexJob{}, mapNoRows(err)
		}
	}
	prior, err = scanDocRangeJob(transaction.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 FOR UPDATE`, repo, hash, base))
	version := int64(0)
	alreadyPending := false
	if err == nil {
		retryValidation, err := s.retryLegacyRangeRejection(bound, prior)
		if err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		alreadyPending = prior.State == "waiting" || prior.State == "running" || prior.State == "retrying"
		ready := false
		if prior.State == "completed" {
			if base != "" {
				_, err := s.readDocRangeBoundary(bound, repo, hash, base)
				if err != nil && !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
					return domain.DocRangeIndexJob{}, err
				}
				ready = err == nil
			} else if visible {
				ready, err = s.HasDocVisibleIndex(bound, repo, hash)
				if err != nil {
					return domain.DocRangeIndexJob{}, err
				}
			} else {
				ready, err = s.HasDocRangeIndex(bound, repo, hash)
				if err != nil {
					return domain.DocRangeIndexJob{}, err
				}
			}
		}
		if !retryValidation && (prior.State == "rejected" && prior.Reason == "invalid_document" ||
			minimum == 0 && (prior.State != "completed" || ready)) {
			if err := transaction.Commit(ctx); err != nil {
				return domain.DocRangeIndexJob{}, err
			}
			return prior, nil
		}
		version = prior.Version + 1
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DocRangeIndexJob{}, err
	}
	var pending int
	if err := transaction.QueryRow(ctx, docRangeJobActiveCountSQL, repo, domain.MaxPendingDocRangeJobs).Scan(&pending); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if pending >= domain.MaxPendingDocRangeJobs && !alreadyPending {
		return domain.DocRangeIndexJob{}, domain.ErrResourceOverloaded
	}
	job, err := scanDocRangeJob(transaction.QueryRow(ctx, `INSERT INTO doc_range_jobs_v1(repo_id,doc_hash,base_hash,state,version,created_at,next_attempt,minimum_range_version,byte_source_version,legacy_validation_revision)
 VALUES($1,$2,$3,'waiting',$4,$5,$5,$6::smallint,CASE WHEN $6::smallint=4 THEN (SELECT doc_range_source_version FROM blobs WHERE hash=$2) END,
 CASE WHEN EXISTS(SELECT 1 FROM snapshots WHERE repo_id=$1 AND id IN ($2,$3) AND doc_hash=id AND doc_identity='') THEN $7 ELSE 0 END)
 ON CONFLICT(repo_id,doc_hash,base_hash) DO UPDATE SET state='waiting',reason='',attempts=0,version=EXCLUDED.version,
 created_at=EXCLUDED.created_at,next_attempt=EXCLUDED.next_attempt,lease_until=NULL,
 minimum_range_version=GREATEST(doc_range_jobs_v1.minimum_range_version,EXCLUDED.minimum_range_version),
 legacy_validation_revision=GREATEST(doc_range_jobs_v1.legacy_validation_revision,EXCLUDED.legacy_validation_revision),
 byte_source_version=CASE WHEN GREATEST(doc_range_jobs_v1.minimum_range_version,EXCLUDED.minimum_range_version)=4
 THEN (SELECT doc_range_source_version FROM blobs WHERE hash=EXCLUDED.doc_hash) END
 RETURNING `+docRangeJobColumns, repo, hash, base, version, now, minimum, legacyRangeValidationRevision))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return job, nil
}

const docRangeClaimCandidateSQL = `SELECT ` + docRangeJobColumns + ` FROM doc_range_jobs_v1 job
 WHERE job.state IN ('waiting','retrying','running') AND job.next_attempt<=$1
	AND job.legacy_validation_revision<=$3
	AND (job.minimum_range_version=0 OR $2 OR job.state='running')
 AND (job.state<>'running' OR (job.lease_until<=$1 AND job.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM doc_range_jobs_v1 other WHERE other.repo_id=job.repo_id AND (other.doc_hash,other.base_hash)<>(job.doc_hash,job.base_hash) AND other.state='running')
 AND ($4::text='' OR job.repo_id=$4)
 AND ($5::boolean OR (job.next_attempt,job.created_at,job.repo_id,job.doc_hash,job.base_hash)>
 ($6::timestamptz,$7::timestamptz,$8::text,$9::text,$10::text))
 ORDER BY job.next_attempt,job.created_at,job.repo_id,job.doc_hash,job.base_hash LIMIT 1`

func (storage *PostgresStore) docRangeClaimArguments(now time.Time, repo domain.ContentHash, cursor domain.DocRangeIndexJob) []any {
	return []any{now, storage.legacyByteRangePublication, legacyRangeValidationRevision, repo, cursor.RepoID == "",
		cursor.NextAttempt, cursor.CreatedAt, cursor.RepoID, cursor.DocHash, cursor.BaseHash}
}

func (s *PostgresStore) ClaimDocRangeJob(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	if now.IsZero() || lease <= 0 {
		return domain.DocRangeIndexJob{}, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if _, borrowed := ctx.Value(repositoryTxKey{}).(*repositoryTx); borrowed || outbound.DocReadOnly(ctx) {
		return domain.DocRangeIndexJob{}, domain.ErrConflict
	}
	var cursor domain.DocRangeIndexJob
	for {
		job, err := scanDocRangeJob(s.pool.QueryRow(ctx, docRangeClaimCandidateSQL, s.docRangeClaimArguments(now, "", cursor)...))
		if err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		claimed, err := s.claimDocRangeCandidate(ctx, now, lease, job)
		if errors.Is(err, errDocRangeJobReleased) {
			return domain.DocRangeIndexJob{}, domain.ErrNotFound
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return claimed, err
		}
		cursor = job
	}
}

func (s *PostgresStore) claimDocRangeCandidate(ctx context.Context, now time.Time, lease time.Duration, job domain.DocRangeIndexJob) (domain.DocRangeIndexJob, error) {
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	defer rollbackPG(transaction)
	epoch, err := retainDocJobRepositoryLockPG(ctx, transaction, job.RepoID, true)
	if err != nil {
		var blocked *pgconn.PgError
		if errors.As(err, &blocked) && blocked.Code == "55P03" {
			return domain.DocRangeIndexJob{}, domain.ErrNotFound
		}
		return domain.DocRangeIndexJob{}, err
	}
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-claim:"+string(job.RepoID)).Scan(&locked); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if !locked {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	job, err = scanDocRangeJob(transaction.QueryRow(ctx, docRangeClaimCandidateSQL+` FOR UPDATE OF job SKIP LOCKED`, s.docRangeClaimArguments(now, job.RepoID, domain.DocRangeIndexJob{})...))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	var other bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND (doc_hash,base_hash)<>($2,$3) AND state='running')`, job.RepoID, job.DocHash, job.BaseHash).Scan(&other); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if other {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	if !s.legacyByteRangePublication {
		tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 AND version=$4 AND minimum_range_version=4
 AND state='running' AND lease_until<=$5 AND lease_until<=clock_timestamp()`, job.RepoID, job.DocHash, job.BaseHash, job.Version, now)
		if err != nil {
			return domain.DocRangeIndexJob{}, err
		}
		if tag.RowsAffected() != 0 {
			if err := transaction.Commit(ctx); err != nil {
				return domain.DocRangeIndexJob{}, err
			}
			return domain.DocRangeIndexJob{}, errDocRangeJobReleased
		}
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='4'`); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if _, err := transaction.Exec(ctx, `SELECT set_config('cxt.legacy_range_validation_revision',$1::integer::text,true)`, legacyRangeValidationRevision); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	job, err = scanDocRangeJob(transaction.QueryRow(ctx, `UPDATE doc_range_jobs_v1
	 SET state='running',reason='',attempts=attempts+1,version=version+1,lease_until=$3,
 legacy_validation_revision=GREATEST(legacy_validation_revision,CASE WHEN EXISTS(
 SELECT 1 FROM snapshots WHERE repo_id=$1 AND id IN ($2,$4) AND doc_hash=id AND doc_identity='') THEN $5 ELSE 0 END)
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$4 AND $3>clock_timestamp() RETURNING `+docRangeJobColumns, job.RepoID, job.DocHash, now.Add(lease), job.BaseHash, legacyRangeValidationRevision))
	if err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	job.RepositoryEpoch = epoch
	return job, nil
}

func lockDocRangeJob(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob) error {
	if err := retainDocRangeJobRepositoryPG(ctx, transaction, job); err != nil {
		return err
	}
	var present int
	err := transaction.QueryRow(ctx, `SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 FOR UPDATE`, job.RepoID, job.DocHash, job.BaseHash).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrConflict
	}
	return err
}

func (s *PostgresStore) RenewDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" || now.IsZero() || lease <= 0 {
		return domain.ErrValidation
	}
	transaction, err := s.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=GREATEST(lease_until,$5)
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$6 AND version=$3 AND state='running' AND lease_until>$4 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Version, now, now.Add(lease), job.BaseHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if (job.State != "retrying" && job.State != "rejected") || now.IsZero() || len(job.Reason) > 128 {
		return domain.ErrValidation
	}
	transaction, err := s.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state=$5,reason=$6,next_attempt=$7,lease_until=NULL
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$8 AND version=$3 AND state='running' AND lease_until>$4 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Version, now, job.State, job.Reason, job.NextAttempt, job.BaseHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) BuildDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob) (result error) {
	work, err := s.PrepareDocRangeWork(ctx, job)
	if err != nil {
		return err
	}
	if work == nil {
		return s.BuildDocRangeCompatibilityJob(ctx, job)
	}
	defer func() {
		if work != nil {
			result = errors.Join(result, work.Close())
		}
	}()
	for {
		progress, err := work.Step(ctx, job)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch progress {
		case outbound.DocRangeWorkPending:
		case outbound.DocRangeWorkPublishable:
			return work.Publish(ctx, job)
		case outbound.DocRangeWorkCompatibilityRequired:
			err := work.Close()
			work = nil
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return s.BuildDocRangeCompatibilityJob(ctx, job)
		default:
			return domain.ErrIntegrity
		}
	}
}

func (s *PostgresStore) BuildDocRangeCompatibilityJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" {
		return domain.ErrValidation
	}
	if job.BaseHash != "" {
		return s.buildDocRangeBoundaryJob(ctx, job)
	}
	transaction, err := s.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	var live bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3 AND lease_until>clock_timestamp())`, job.RepoID, job.DocHash, job.Version).Scan(&live); err != nil {
		return err
	}
	if !live {
		return domain.ErrConflict
	}
	var minimum int
	if err := transaction.QueryRow(ctx, `SELECT minimum_range_version FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND version=$3`, job.RepoID, job.DocHash, job.Version).Scan(&minimum); err != nil {
		return mapNoRows(err)
	}
	if minimum >= 4 && !s.legacyByteRangePublication {
		return domain.ErrConflict
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='4'`); err != nil {
		return err
	}
	var identity domain.DocumentIdentity
	if err := transaction.QueryRow(ctx, docRangeJobIdentitySQL+` FOR SHARE OF snapshot`, job.RepoID, job.DocHash, domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy).Scan(&identity); err != nil {
		return mapNoRows(err)
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: s, repo: job.RepoID})
	if identity == domain.DocumentIdentityLegacy {
		err = s.ensureLegacyRangeGenerationMinimum(bound, job.RepoID, job.DocHash, minimum)
	} else {
		err = s.ensureRangeGeneration(bound, job.RepoID, job.DocHash)
	}
	if err != nil {
		return err
	}
	generation, err := s.readRangeGeneration(bound, job.RepoID, job.DocHash)
	if err != nil {
		return err
	}
	if generation.Identity() != identity {
		return domain.ErrIntegrity
	}
	if generation.ByteStorage() == domain.DocRangeByteStorageDerived {
		if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='5'`); err != nil {
			return err
		}
	}
	if minimum >= 4 {
		if generation.Version < 4 && generation.StreamBytes != 0 {
			return domain.ErrDocByteRangeBackfillRequired
		}
		if _, err := transaction.Exec(ctx, `UPDATE doc_range_generations_v2 SET range_layout_version=range_layout_version WHERE doc_hash=$1`, job.DocHash); err != nil {
			return err
		}
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state='completed',reason='',lease_until=NULL,
 byte_source_version=CASE WHEN minimum_range_version=4 THEN (SELECT doc_range_source_version FROM blobs WHERE hash=$2) END
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

var _ outbound.DocRangeReadinessStore = (*PostgresStore)(nil)
var _ outbound.DocRangeJobStore = (*PostgresStore)(nil)
