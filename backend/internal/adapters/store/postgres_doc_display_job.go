//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) EnqueueDocDisplay(ctx context.Context, target domain.DocDisplayTarget, now time.Time) (queued domain.DocDisplayJob, failure error) {
	if err := target.Validate(); err != nil {
		return queued, err
	}
	if now.IsZero() {
		return queued, domain.ErrValidation
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = docRangeEnqueueError(parent, failure) }()
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return queued, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
		return queued, err
	}
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-display-enqueue:"+string(target.RepoID)); err != nil {
		return queued, err
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: s, repo: target.RepoID})
	target, err = s.proveDocDisplayTarget(bound, target, true)
	if err != nil {
		return queued, err
	}
	protocol := 2
	if target.DisplayProfile() == domain.DocDisplayProjectionProfile {
		protocol = 5
	}
	prior, err := scanDocDisplayJob(transaction.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE `+docDisplayKey(target)+` FOR UPDATE`,
		target.RepoID, target.DocHash, target.Event.Index))
	if errors.Is(err, domain.ErrNotFound) {
		if err := checkDocDisplayCapacity(ctx, transaction, target.RepoID); err != nil {
			return queued, err
		}
		queued, err = scanDocDisplayJob(transaction.QueryRow(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,state,version,created_at,next_attempt,worker_protocol)
 VALUES($1,$2,$14,$3,$4,$5,$6,$7,$8,$9,$10,$11,'waiting',0,$12,$12,$13)
 ON CONFLICT(repo_id,doc_hash,event_index,profile) DO NOTHING RETURNING `+docDisplayJobColumns,
			target.RepoID, target.DocHash, target.Event.Index, target.Event.Hash, target.Event.Offset, target.Event.Length,
			target.Event.Seq, target.Event.Role, target.Binding.SourceID, target.Binding.SourceVersion, target.Binding.RangeGeneration,
			now, protocol, target.DisplayProfile()))
		if err == nil {
			return queued, transaction.Commit(ctx)
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.DocDisplayJob{}, err
		}
		prior, err = scanDocDisplayJob(transaction.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE `+docDisplayKey(target)+` FOR UPDATE`,
			target.RepoID, target.DocHash, target.Event.Index))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.DocDisplayJob{}, domain.ErrConflict
		}
	}
	if err != nil {
		return queued, err
	}
	reopensCompleted := sameDocDisplayTarget(prior.Target, target)
	if reopensCompleted {
		if prior.State != "completed" {
			return prior, transaction.Commit(ctx)
		}
		var present bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2)`, prior.Target.Event.Hash, target.DisplayProfile()).Scan(&present); err != nil {
			return queued, err
		}
		if present {
			return prior, transaction.Commit(ctx)
		}
	}
	version := prior.Version + 1
	replacesActive := prior.State == "waiting" || prior.State == "running" || prior.State == "retrying"
	if !replacesActive {
		if err := checkDocDisplayCapacity(ctx, transaction, target.RepoID); err != nil {
			return queued, err
		}
	}
	if reopensCompleted {
		preparation := `source_hash IS NOT NULL`
		if target.DisplayProfile() == domain.DocDisplayProjectionProfile {
			preparation = `preparation_hash IS NOT NULL AND preparation IS NOT NULL`
		}
		queued, err = scanDocDisplayJob(transaction.QueryRow(ctx, `UPDATE doc_display_jobs_v1
 SET state='waiting',reason='',attempts=0,version=$4,next_attempt=$5,lease_until=NULL
 WHERE `+docDisplayKey(target)+` AND `+preparation+` RETURNING `+docDisplayJobColumns,
			target.RepoID, target.DocHash, target.Event.Index, version, now))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.DocDisplayJob{}, domain.ErrIntegrity
		}
		if err != nil {
			return domain.DocDisplayJob{}, err
		}
		return queued, transaction.Commit(ctx)
	}
	queued, err = scanDocDisplayJob(transaction.QueryRow(ctx, `UPDATE doc_display_jobs_v1 SET event_hash=$4,event_offset=$5,
 event_length=$6,event_seq=$7,event_role=$8,source_id=$9,source_version=$10,range_generation=$11,
 state='waiting',reason='',attempts=0,version=$12,created_at=$13,next_attempt=$13,lease_until=NULL,
 source_hash=NULL,catalog_epoch=NULL,checkpoint_hash=NULL,checkpoint=NULL,discovery_hash=NULL,discovery=NULL,
 preparation_hash=NULL,preparation=NULL,worker_protocol=$14 WHERE `+docDisplayKey(target)+` RETURNING `+docDisplayJobColumns,
		target.RepoID, target.DocHash, target.Event.Index, target.Event.Hash, target.Event.Offset, target.Event.Length,
		target.Event.Seq, target.Event.Role, target.Binding.SourceID, target.Binding.SourceVersion, target.Binding.RangeGeneration, version, now,
		protocol))
	if err != nil {
		return domain.DocDisplayJob{}, err
	}
	return queued, transaction.Commit(ctx)
}

func checkDocDisplayCapacity(ctx context.Context, transaction pgx.Tx, repository domain.ContentHash) error {
	var active int
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND state IN ('waiting','running','retrying') LIMIT $2) active`, repository, domain.MaxPendingDocRangeJobs).Scan(&active); err != nil {
		return err
	}
	if active >= domain.MaxPendingDocRangeJobs {
		return domain.ErrResourceOverloaded
	}
	return nil
}

func (s *PostgresStore) ClaimDocDisplay(ctx context.Context, now time.Time, lease time.Duration) (domain.DocDisplayJob, error) {
	if now.IsZero() || lease <= 0 {
		return domain.DocDisplayJob{}, domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return domain.DocDisplayJob{}, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='5'; SET LOCAL cxthub.doc_catalog_protocol='3'`); err != nil {
		return domain.DocDisplayJob{}, err
	}
	job, err := scanDocDisplayJob(transaction.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 job
 WHERE ((profile='tool-result-string-v1' AND worker_protocol=2) OR (profile='public-event-display-v1' AND worker_protocol=5))
 AND state IN ('waiting','retrying','running') AND next_attempt<=$1 AND next_attempt<=clock_timestamp()
 AND (state<>'running' OR (lease_until<=$1 AND lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM doc_display_jobs_v1 other WHERE other.repo_id=job.repo_id
 AND (other.doc_hash,other.event_index,other.profile)<>(job.doc_hash,job.event_index,job.profile) AND other.state='running')
 ORDER BY next_attempt,created_at,repo_id,doc_hash,event_index FOR UPDATE OF job SKIP LOCKED LIMIT 1`, now))
	if err != nil {
		return domain.DocDisplayJob{}, err
	}
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-display-claim:"+string(job.RepoID)).Scan(&locked); err != nil {
		return domain.DocDisplayJob{}, err
	}
	if !locked {
		return domain.DocDisplayJob{}, domain.ErrNotFound
	}
	var other bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_jobs_v1 WHERE repo_id=$1
 AND (doc_hash,event_index,profile)<>($2,$3,$4) AND state='running')`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).Scan(&other); err != nil {
		return domain.DocDisplayJob{}, err
	}
	if other {
		return domain.DocDisplayJob{}, domain.ErrNotFound
	}
	job, err = scanDocDisplayJob(transaction.QueryRow(ctx, `UPDATE doc_display_jobs_v1 SET state='running',reason='',attempts=attempts+1,
 version=version+1,lease_until=$4 WHERE `+docDisplayKey(job.Target)+`
 AND ((profile='tool-result-string-v1' AND worker_protocol=2) OR (profile='public-event-display-v1' AND worker_protocol=5))
 AND $4>clock_timestamp() RETURNING `+docDisplayJobColumns,
		job.RepoID, job.DocHash, job.Target.Event.Index, now.Add(lease)))
	if err != nil {
		return domain.DocDisplayJob{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocDisplayJob{}, err
	}
	return job, nil
}

func (s *PostgresStore) RenewDocDisplay(ctx context.Context, job domain.DocDisplayJob, now time.Time, lease time.Duration) (failure error) {
	defer func() { failure = docCatalogWriterError(failure) }()
	if job.Validate() != nil || job.State != "running" || now.IsZero() || lease <= 0 {
		return domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocDisplayJob(ctx, transaction, job); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET lease_until=GREATEST(lease_until,$6)
 WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>$5 AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, now, now.Add(lease))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) FinishDocDisplay(ctx context.Context, job domain.DocDisplayJob, now time.Time) error {
	if job.Validate() != nil || (job.State != "waiting" && job.State != "retrying" && job.State != "rejected") || now.IsZero() || len(job.Reason) > 128 {
		return domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocDisplayJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET state=$6,reason=$7,next_attempt=$8,lease_until=NULL
 WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>$5 AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, now, job.State, job.Reason, job.NextAttempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}
