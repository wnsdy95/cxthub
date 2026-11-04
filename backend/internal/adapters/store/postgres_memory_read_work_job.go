//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const memoryReadWorkClaimSQL = `SELECT job.repo_id,job.target_id FROM memory_read_work job
 JOIN memory_read_work_scopes scope ON scope.repo_id=job.repo_id AND scope.enabled
 WHERE job.repo_id=ANY($1::text[]) AND job.state IN ('waiting','retrying','running') AND job.next_attempt<=clock_timestamp()
 AND NOT(job.target_id=ANY($2::text[]))
 AND (job.state<>'running' OR job.lease_until<=clock_timestamp())
 ORDER BY job.next_attempt,job.created_at,job.repo_id,job.target_id
 FOR UPDATE OF job SKIP LOCKED FOR SHARE OF scope SKIP LOCKED LIMIT 1`

func memoryReadWorkLeaseValid(lease time.Duration) bool {
	return lease >= time.Millisecond && lease <= 5*time.Minute
}

func (storage *PostgresStore) ClaimMemoryReadWork(ctx context.Context, repos []domain.ContentHash, lease time.Duration) (work domain.MemoryReadWork, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if !memoryReadWorkLeaseValid(lease) || len(repos) > domain.MaxMemoryReadWorkerRepos {
		return work, domain.ErrValidation
	}
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return work, err
		}
	}
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return work, err
	}
	if outbound.DocReadOnly(ctx) {
		return work, domain.ErrConflict
	}
	if len(repos) == 0 {
		return work, domain.ErrNotFound
	}
	skipped := []string{}
	for range domain.MaxPendingMemoryReadWork {
		claimed, identifier, err := storage.claimMemoryReadWorkCandidate(ctx, repos, skipped, lease)
		if err != nil {
			return work, err
		}
		if claimed.State == "running" {
			return claimed, nil
		}
		skipped = append(skipped, string(identifier))
	}
	return work, domain.ErrNotFound
}

func (storage *PostgresStore) claimMemoryReadWorkCandidate(ctx context.Context, repos []domain.ContentHash, skipped []string, lease time.Duration) (domain.MemoryReadWork, domain.ContentHash, error) {
	var work domain.MemoryReadWork
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return work, "", err
	}
	defer rollbackPG(transaction)
	var repo, identifier domain.ContentHash
	if err := transaction.QueryRow(ctx, memoryReadWorkClaimSQL, strs(repos), skipped).Scan(&repo, &identifier); err != nil {
		return work, "", mapNoRows(err)
	}
	if err := lockMemoryReadWorkScope(ctx, transaction, repo, false); err != nil {
		var databaseError *pgconn.PgError
		if errors.Is(err, domain.ErrConflict) || (errors.As(err, &databaseError) && databaseError.Code == "55P03") {
			return work, identifier, nil
		}
		return work, identifier, err
	}
	sourceCheck, err := transaction.Begin(ctx)
	if err != nil {
		return work, identifier, err
	}
	if _, err := sourceCheck.Exec(ctx, `SET LOCAL statement_timeout='100ms'`); err != nil {
		return work, identifier, err
	}
	prior, sourceErr := scanMemoryReadWork(ctx, sourceCheck.QueryRow(ctx, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(repo), string(identifier)), repo, identifier)
	var source domain.RepositoryMemorySourceGeneration
	if sourceErr == nil {
		source, sourceErr = readMemoryQualificationSource(ctx, sourceCheck, repo)
	}
	if sourceErr != nil {
		if err := sourceCheck.Rollback(ctx); err != nil {
			return work, identifier, err
		}
		if ctx.Err() != nil {
			return work, identifier, ctx.Err()
		}
		reason, retry := memoryReadWorkClaimFailure(sourceErr)
		if _, err := transaction.Exec(ctx, `UPDATE memory_read_work SET
 state=CASE WHEN $4 AND attempts<5 THEN 'retrying' ELSE 'rejected' END,
 reason=CASE WHEN $4 AND attempts>=5 THEN 'attempts_exhausted' ELSE $3 END,
 attempts=LEAST(attempts+1,6),version=nextval('memory_read_work_fence_seq'),lease_until=NULL,
 updated_at=clock_timestamp(),next_attempt=clock_timestamp()+power(2,LEAST(attempts,5))*interval '1 second'
 WHERE repo_id=$1 AND target_id=$2`, string(repo), string(identifier), reason, retry); err != nil {
			return work, identifier, err
		}
		return work, identifier, transaction.Commit(ctx)
	}
	if err := sourceCheck.Commit(ctx); err != nil {
		return work, identifier, err
	}
	attempts := prior.Attempts
	if source != prior.Source {
		attempts = 0
	}
	if attempts == domain.MaxMemoryReadWorkAttempts {
		if _, err := transaction.Exec(ctx, `UPDATE memory_read_work SET state='rejected',reason='attempts_exhausted',
 version=nextval('memory_read_work_fence_seq'),lease_until=NULL,updated_at=clock_timestamp()
 WHERE repo_id=$1 AND target_id=$2`, string(repo), string(identifier)); err != nil {
			return work, identifier, err
		}
		return work, identifier, transaction.Commit(ctx)
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return work, identifier, err
	}
	work, err = scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `WITH stamp AS (SELECT clock_timestamp() AS now)
 UPDATE memory_read_work SET state='running',reason='',source=$3,attempts=$4,version=nextval('memory_read_work_fence_seq'),
 updated_at=stamp.now,lease_until=stamp.now+$5*interval '1 microsecond',next_attempt=stamp.now+$5*interval '1 microsecond'
 FROM stamp WHERE repo_id=$1 AND target_id=$2 RETURNING `+memoryReadWorkColumns,
		string(repo), string(identifier), raw, attempts+1, lease.Microseconds()), repo, identifier)
	if err != nil {
		return work, identifier, err
	}
	return work, identifier, transaction.Commit(ctx)
}

func memoryReadWorkClaimFailure(err error) (string, bool) {
	switch {
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		return "identity_upgrade_required", true
	case errors.Is(err, domain.ErrCatalogResetRequired):
		return "source_reset", true
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNotFound):
		return "invalid_source", false
	case errors.Is(err, domain.ErrMemoryProjectionLimit):
		return "resource_limit", false
	default:
		return "temporary_failure", true
	}
}

func lockMemoryReadWork(ctx context.Context, transaction pgx.Tx, job domain.MemoryReadWork) (domain.MemoryReadWork, error) {
	current, err := lockMemoryReadWorkIdentity(ctx, transaction, job)
	if err != nil {
		return current, err
	}
	if current.Source != job.Source {
		return current, domain.ErrConflict
	}
	return current, nil
}

func lockMemoryReadWorkIdentity(ctx context.Context, transaction pgx.Tx, job domain.MemoryReadWork) (domain.MemoryReadWork, error) {
	if err := lockMemoryReadWorkScope(ctx, transaction, job.Target.RepoID, false); err != nil {
		return domain.MemoryReadWork{}, err
	}
	current, err := scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2 FOR UPDATE`, string(job.Target.RepoID), string(job.ID)), job.Target.RepoID, job.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return current, domain.ErrConflict
	}
	if err != nil {
		return current, err
	}
	if current.Version != job.Version || current.Target != job.Target {
		return current, domain.ErrConflict
	}
	return current, nil
}

func memoryReadWorkLive(ctx context.Context, transaction pgx.Tx, job domain.MemoryReadWork) error {
	var live bool
	if err := transaction.QueryRow(ctx, `SELECT state='running' AND version=$3 AND lease_until>clock_timestamp()
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID), job.Version).Scan(&live); err != nil {
		return mapNoRows(err)
	}
	if !live {
		return domain.ErrConflict
	}
	return nil
}

func (storage *PostgresStore) RenewMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, lease time.Duration) (failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" || !memoryReadWorkLeaseValid(lease) {
		return domain.ErrValidation
	}
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := lockMemoryReadWork(ctx, transaction, job); err != nil {
		return err
	}
	if err := fenceMemoryQualificationSource(ctx, transaction, job.Source); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE memory_read_work SET lease_until=GREATEST(lease_until,clock_timestamp()+$4*interval '1 microsecond'),
 next_attempt=GREATEST(lease_until,clock_timestamp()+$4*interval '1 microsecond'),updated_at=clock_timestamp()
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='running' AND lease_until>clock_timestamp()`,
		string(job.Target.RepoID), string(job.ID), job.Version, lease.Microseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) FailMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, reason string, retry bool) (failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" || reason == "" || !domain.MemoryReadWorkReasonValid(reason) || (reason == "retention_cleanup" && !retry) {
		return domain.ErrValidation
	}
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	current, err := lockMemoryReadWork(ctx, transaction, job)
	if err != nil {
		return err
	}
	source, err := readMemoryQualificationSource(ctx, transaction, job.Target.RepoID)
	if err != nil {
		return err
	}
	state := "rejected"
	attempts := current.Attempts
	if source != job.Source {
		retry, reason = true, "source_changed"
	}
	if reason == "retention_cleanup" {
		attempts--
	}
	if retry {
		state = "retrying"
		if attempts == domain.MaxMemoryReadWorkAttempts && source == job.Source {
			state, reason = "rejected", "attempts_exhausted"
		}
	}
	delay := min(1<<max(0, current.Attempts-1), 32)
	if reason == "retention_cleanup" {
		delay = 1
	}
	tag, err := transaction.Exec(ctx, `UPDATE memory_read_work SET state=$4,reason=$5,lease_until=NULL,attempts=$7,
 updated_at=clock_timestamp(),next_attempt=clock_timestamp()+$6*interval '1 second'
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='running' AND lease_until>clock_timestamp()`,
		string(job.Target.RepoID), string(job.ID), job.Version, state, reason, delay, attempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) FinishMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, qualification domain.MemoryReadQualification) (identity domain.ContentHash, failure error) {
	return storage.finishMemoryReadWork(ctx, job, qualification, false)
}

func (storage *PostgresStore) FinishMinimumMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, qualification domain.MemoryReadQualification) (identity domain.ContentHash, failure error) {
	return storage.finishMemoryReadWork(ctx, job, qualification, true)
}

func (storage *PostgresStore) finishMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, qualification domain.MemoryReadQualification, bounded bool) (identity domain.ContentHash, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if err := job.Validate(); err != nil {
		return "", err
	}
	if job.State != "running" && job.State != "completed" {
		return "", domain.ErrValidation
	}
	raw, err := domain.MemoryReadQualificationBytes(ctx, qualification)
	if err != nil {
		return "", err
	}
	var owned domain.MemoryReadQualification
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &owned); err != nil {
		return "", err
	}
	var minimum *domain.RepositoryMemorySourceGeneration
	if bounded {
		if err := domain.CheckMemorySourceMinimum(owned.Source, job.Source); err != nil {
			return "", err
		}
		minimum = &job.Source
	} else if owned.Source != job.Source {
		return "", domain.ErrConflict
	}
	identifier := domain.HashContent(raw)
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return "", err
	}
	defer rollbackPG(transaction)
	current, err := lockMemoryReadWorkIdentity(ctx, transaction, job)
	if err != nil {
		return "", err
	}
	duplicate := current.State == "completed" && current.QualificationID == identifier
	if current.Source != job.Source && !(bounded && duplicate && current.Source == owned.Source) {
		return "", domain.ErrConflict
	}
	if !duplicate {
		if err := memoryReadWorkLive(ctx, transaction, job); err != nil {
			return "", err
		}
	}
	identifier, latest, err := qualifyMemoryReadAtMinimumInTx(ctx, transaction, owned, raw, &job.Target, minimum)
	if err != nil {
		return "", err
	}
	if bounded {
		header, _, err := readMemoryReadDescriptor(ctx, transaction, owned.Source.Graph.RepoID, owned.ReadID)
		if err != nil {
			return "", err
		}
		if err := requireMemoryReadComparison(ctx, transaction, owned.ReadID, job.ID, header, true); err != nil {
			return "", err
		}
	}
	if !duplicate {
		source, err := json.Marshal(owned.Source)
		if err != nil {
			return "", err
		}
		tag, err := transaction.Exec(ctx, `UPDATE memory_read_work SET state='completed',reason='',qualification_id=$4,
 source=$5,wake_revision=CASE WHEN $6 THEN COALESCE((SELECT revision FROM memory_read_wakes WHERE repo_id=$1),wake_revision) ELSE wake_revision END,
 lease_until=NULL,updated_at=clock_timestamp()
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='running' AND lease_until>clock_timestamp()`,
			string(job.Target.RepoID), string(job.ID), job.Version, string(identifier), source, latest)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() != 1 {
			return "", domain.ErrConflict
		}
	} else {
		if _, err := transaction.Exec(ctx, `UPDATE memory_read_work SET qualification_id=qualification_id
 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID)); err != nil {
			return "", err
		}
	}
	if !bounded {
		if _, err := transaction.Exec(ctx, `SELECT cxt_schedule_memory_read_wake($1,clock_timestamp())
 WHERE NOT EXISTS(SELECT 1 FROM memory_read_comparisons WHERE repo_id=$1 AND read_id=$2)`,
			string(job.Target.RepoID), string(owned.ReadID)); err != nil {
			return "", err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return "", err
	}
	return identifier, nil
}
