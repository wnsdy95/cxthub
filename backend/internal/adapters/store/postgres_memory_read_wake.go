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

var _ outbound.MemoryReadWorkReconciler = (*PostgresStore)(nil)

const memoryReadWakeJoins = ` FROM memory_read_work job
 LEFT JOIN memory_read_qualifications qualified ON qualified.repo_id=job.repo_id AND qualified.qualification_id=job.qualification_id
 LEFT JOIN memory_read_headers result ON result.repo_id=qualified.repo_id AND result.read_id=qualified.read_id
 LEFT JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
 LEFT JOIN memory_read_comparisons comparison ON comparison.repo_id=result.repo_id AND comparison.read_id=result.read_id `

const memoryReadWakeEligible = `job.repo_id=$1 AND job.state IN ('completed','rejected') AND (
 (job.wake_revision<$3 AND (job.source<>$2 OR job.wake_revision<$4)) OR
 (job.state='completed' AND (comparison.read_id IS NULL OR capture.expires_at IS NULL OR capture.expires_at<=clock_timestamp())))`

func (storage *PostgresStore) ReconcileMemoryReadWork(ctx context.Context, repos []domain.ContentHash, limit int) (int, error) {
	if limit < 1 || limit > domain.MaxPendingMemoryReadWork || len(repos) > domain.MaxMemoryReadWorkerRepos {
		return 0, domain.ErrValidation
	}
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return 0, err
		}
	}
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return 0, err
	}
	if outbound.DocReadOnly(ctx) {
		return 0, domain.ErrConflict
	}
	if len(repos) == 0 {
		return 0, nil
	}
	completed := 0
	skipped := []string{}
	for range limit {
		repo, processed, err := storage.reconcileMemoryReadWake(ctx, repos, skipped)
		if errors.Is(err, domain.ErrNotFound) {
			return completed, nil
		}
		if err != nil {
			return completed, err
		}
		skipped = append(skipped, string(repo))
		if processed {
			completed++
		}
	}
	return completed, nil
}

func (storage *PostgresStore) reconcileMemoryReadWake(ctx context.Context, repos []domain.ContentHash, skipped []string) (repo domain.ContentHash, processed bool, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return repo, false, err
	}
	defer rollbackPG(transaction)
	var revision, objects int64
	if err := transaction.QueryRow(ctx, `SELECT wake.repo_id,wake.revision,wake.object_revision
 FROM memory_read_wakes wake JOIN memory_read_work_scopes scope ON scope.repo_id=wake.repo_id AND scope.enabled
 WHERE wake.repo_id=ANY($1::text[]) AND NOT(wake.repo_id=ANY($2::text[])) AND wake.next_attempt<=clock_timestamp()
 ORDER BY wake.next_attempt,wake.repo_id FOR UPDATE OF wake,scope SKIP LOCKED LIMIT 1`,
		strs(repos), skipped).Scan(&repo, &revision, &objects); err != nil {
		return repo, false, mapNoRows(err)
	}
	if err := lockMemoryReadWorkScope(ctx, transaction, repo, true); err != nil {
		var databaseError *pgconn.PgError
		if errors.Is(err, domain.ErrConflict) || (errors.As(err, &databaseError) && databaseError.Code == "55P03") {
			return repo, false, nil
		}
		return repo, false, err
	}
	check, err := transaction.Begin(ctx)
	if err != nil {
		return repo, false, err
	}
	if _, err := check.Exec(ctx, `SET LOCAL statement_timeout='100ms'`); err != nil {
		return repo, false, err
	}
	source, sourceErr := readMemoryQualificationSource(ctx, check, repo)
	if sourceErr != nil {
		if err := check.Rollback(ctx); err != nil {
			return repo, false, err
		}
		if err := deferMemoryReadWake(ctx, transaction, repo, revision, sourceErr); err != nil {
			return repo, false, err
		}
		return repo, true, transaction.Commit(ctx)
	}
	if err := check.Commit(ctx); err != nil {
		return repo, false, err
	}
	check, err = transaction.Begin(ctx)
	if err != nil {
		return repo, false, err
	}
	if _, err := check.Exec(ctx, `SET LOCAL statement_timeout='250ms'`); err != nil {
		return repo, false, err
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return repo, false, err
	}
	if targetErr := reconcileMemoryReadTargets(ctx, check, repo, raw, revision, objects); targetErr != nil {
		if err := check.Rollback(ctx); err != nil {
			return repo, false, err
		}
		if err := deferMemoryReadWake(ctx, transaction, repo, revision, targetErr); err != nil {
			return repo, false, err
		}
		return repo, true, transaction.Commit(ctx)
	}
	if err := check.Commit(ctx); err != nil {
		return repo, false, err
	}
	return repo, true, transaction.Commit(ctx)
}

func deferMemoryReadWake(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, revision int64, failure error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reason, retry := memoryReadWorkClaimFailure(failure)
	tag, err := transaction.Exec(ctx, `UPDATE memory_read_wakes SET
 next_attempt=CASE WHEN $4 AND (attempts<5 OR $3='temporary_failure')
 THEN clock_timestamp()+power(2,LEAST(attempts,5))*interval '1 second' END,
 reason=CASE WHEN $4 AND attempts>=5 AND $3<>'temporary_failure' THEN 'attempts_exhausted' ELSE $3 END,
 attempts=LEAST(attempts+1,6) WHERE repo_id=$1 AND revision=$2`, string(repo), revision, reason, retry)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func reconcileMemoryReadTargets(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, source []byte, revision, objects int64) error {
	if _, err := transaction.Exec(ctx, `UPDATE memory_read_work SET
 attempts=CASE WHEN source<>$2 OR wake_revision<$4 THEN 0 ELSE attempts END,
 reason=CASE WHEN source<>$2 OR wake_revision<$4 THEN '' ELSE reason END,
 next_attempt=CASE WHEN source<>$2 OR wake_revision<$4 THEN clock_timestamp() ELSE next_attempt END,
 version=CASE WHEN source<>$2 OR wake_revision<$4 THEN nextval('memory_read_work_fence_seq') ELSE version END,
 wake_revision=$3 WHERE repo_id=$1 AND state IN ('waiting','retrying') AND wake_revision<$3`,
		string(repo), source, revision, objects); err != nil {
		return err
	}
	var active int
	if err := transaction.QueryRow(ctx, memoryReadWorkActiveCountSQL, string(repo), domain.MaxPendingMemoryReadWork).Scan(&active); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `WITH candidates AS (
 SELECT job.target_id `+memoryReadWakeJoins+` WHERE `+memoryReadWakeEligible+`
 ORDER BY job.updated_at,job.target_id LIMIT $5 FOR UPDATE OF job)
 UPDATE memory_read_work job SET state='waiting',reason='',attempts=0,
 version=nextval('memory_read_work_fence_seq'),lease_until=NULL,wake_revision=$3,
 next_attempt=clock_timestamp(),updated_at=clock_timestamp()
 FROM candidates WHERE job.repo_id=$1 AND job.target_id=candidates.target_id`,
		string(repo), source, revision, objects, domain.MaxPendingMemoryReadWork-active); err != nil {
		return err
	}
	var remaining bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 `+memoryReadWakeJoins+` WHERE `+memoryReadWakeEligible+`)`,
		string(repo), source, revision, objects).Scan(&remaining); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `UPDATE memory_read_work retained SET wake_revision=$3
 WHERE retained.repo_id=$1 AND retained.state IN ('completed','rejected') AND retained.wake_revision<$3
 AND NOT EXISTS(SELECT 1 `+memoryReadWakeJoins+` WHERE `+memoryReadWakeEligible+` AND job.target_id=retained.target_id)`,
		string(repo), source, revision, objects); err != nil {
		return err
	}
	var due *time.Time
	if remaining {
		if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()+interval '1 second'`).Scan(&due); err != nil {
			return err
		}
	} else if err := transaction.QueryRow(ctx, `SELECT min(CASE WHEN comparison.read_id IS NULL THEN clock_timestamp()
 ELSE COALESCE(capture.expires_at,clock_timestamp()) END) `+memoryReadWakeJoins+`
 WHERE job.repo_id=$1 AND job.state='completed'`, string(repo)).Scan(&due); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE memory_read_wakes SET next_attempt=$3,attempts=0,reason=''
 WHERE repo_id=$1 AND revision=$2`, string(repo), revision, due)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
