//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MemoryReadWorkStore = (*PostgresStore)(nil)
var _ outbound.MemoryReadDemandStore = (*PostgresStore)(nil)
var _ outbound.MinimumMemoryReadWorkStore = (*PostgresStore)(nil)
var _ outbound.CurrentMemoryReadStore = (*PostgresStore)(nil)

const memoryReadWorkColumns = `CASE WHEN octet_length(repo_id)=71 THEN repo_id END,CASE WHEN octet_length(target_id)=71 THEN target_id END,
 CASE WHEN octet_length(target)<=16384 THEN target END,CASE WHEN octet_length(source)<=1024 THEN source END,
 CASE WHEN octet_length(state)<=16 THEN state END,CASE WHEN octet_length(reason)<=32 THEN reason END,
 attempts,version,octet_length(qualification_id),CASE WHEN octet_length(qualification_id)=71 THEN qualification_id END,
 created_at,updated_at,next_attempt,lease_until`

const memoryReadWorkActiveCountSQL = `SELECT count(*) FROM (
 SELECT 1 FROM memory_read_work WHERE repo_id=$1 AND state IN ('waiting','running','retrying') LIMIT $2) active`

func memoryReadWorkError(parent context.Context, failure error) error {
	failure = docRangeEnqueueError(parent, failure)
	var databaseError *pgconn.PgError
	if errors.As(failure, &databaseError) && (databaseError.Code == "40001" || databaseError.Code == "40P01") {
		return domain.ErrConflict
	}
	if errors.As(failure, &databaseError) && databaseError.ConstraintName == "memory_read_work_live" {
		return domain.ErrConflict
	}
	if errors.As(failure, &databaseError) && (databaseError.ConstraintName == "memory_read_work_capture_live" || databaseError.ConstraintName == "memory_read_qualification_capture_live") {
		return domain.ErrMemoryCaptureExpired
	}
	return failure
}

func (storage *PostgresStore) beginMemoryReadWork(ctx context.Context) (pgx.Tx, error) {
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return nil, err
	}
	if outbound.DocReadOnly(ctx) {
		return nil, domain.ErrConflict
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
		rollbackPG(transaction)
		return nil, err
	}
	return transaction, nil
}

func scanMemoryReadWork(ctx context.Context, row pgx.Row, repo, identifier domain.ContentHash) (domain.MemoryReadWork, error) {
	var work domain.MemoryReadWork
	var target, source []byte
	var storedRepo, storedID, qualification *string
	var state, reason *string
	var qualificationBytes *int
	var lease *time.Time
	if err := row.Scan(&storedRepo, &storedID, &target, &source, &state, &reason, &work.Attempts, &work.Version,
		&qualificationBytes, &qualification, &work.CreatedAt, &work.UpdatedAt, &work.NextAttempt, &lease); err != nil {
		return work, mapNoRows(err)
	}
	if storedRepo == nil || storedID == nil || state == nil || reason == nil || len(target) == 0 || len(source) == 0 ||
		(qualificationBytes != nil && (*qualificationBytes != 71 || qualification == nil)) {
		return work, domain.ErrIntegrity
	}
	work.State, work.Reason = *state, *reason
	work.ID = domain.ContentHash(*storedID)
	if qualification != nil {
		work.QualificationID = domain.ContentHash(*qualification)
	}
	if lease != nil {
		work.LeaseUntil = *lease
	}
	if err := domain.DecodeRepositorySourceJSON(ctx, target, &work.Target); err != nil {
		if ctx.Err() != nil {
			return work, ctx.Err()
		}
		return work, domain.ErrIntegrity
	}
	if err := domain.DecodeRepositorySourceJSON(ctx, source, &work.Source); err != nil {
		if ctx.Err() != nil {
			return work, ctx.Err()
		}
		return work, domain.ErrIntegrity
	}
	canonical, err := domain.MemoryReadTargetBytes(ctx, work.Target)
	if err != nil {
		if ctx.Err() != nil {
			return work, ctx.Err()
		}
		return work, domain.ErrIntegrity
	}
	canonicalSource, err := json.Marshal(work.Source)
	if err != nil {
		return work, err
	}
	if !bytes.Equal(canonical, target) || !bytes.Equal(canonicalSource, source) || work.Target.RepoID != domain.ContentHash(*storedRepo) ||
		(repo != "" && work.Target.RepoID != repo) || (identifier != "" && work.ID != identifier) {
		return work, domain.ErrIntegrity
	}
	return work, work.Validate()
}

func lockMemoryReadWorkScope(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, exclusive bool) error {
	query := `SELECT enabled FROM memory_read_work_scopes WHERE repo_id=$1 FOR SHARE NOWAIT`
	if exclusive {
		query = `SELECT enabled FROM memory_read_work_scopes WHERE repo_id=$1 FOR UPDATE`
	}
	var enabled bool
	if err := transaction.QueryRow(ctx, query, string(repo)).Scan(&enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		return err
	}
	if !enabled {
		return domain.ErrConflict
	}
	return nil
}

func (storage *PostgresStore) ConfigureMemoryReadWork(ctx context.Context, repo domain.ContentHash, enabled bool) (failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	var present string
	if err := transaction.QueryRow(ctx, `SELECT id FROM repos WHERE id=$1 FOR KEY SHARE`, string(repo)).Scan(&present); err != nil {
		return mapNoRows(err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_read_work_scopes(repo_id,enabled) VALUES($1,$2)
 ON CONFLICT(repo_id) DO UPDATE SET enabled=EXCLUDED.enabled`, string(repo), enabled); err != nil {
		return err
	}
	if !enabled {
		if _, err := transaction.Exec(ctx, `UPDATE memory_read_work SET state='retrying',reason='temporary_failure',
 version=nextval('memory_read_work_fence_seq'),lease_until=NULL,updated_at=clock_timestamp(),next_attempt=clock_timestamp()
 WHERE repo_id=$1 AND state='running'`, string(repo)); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) RequestMemoryReadWork(ctx context.Context, target domain.MemoryReadTarget) (work domain.MemoryReadWork, failure error) {
	return storage.requestMemoryReadWork(ctx, target, nil)
}

func (storage *PostgresStore) RequestMinimumMemoryReadWork(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement) (work domain.MemoryReadWork, failure error) {
	return storage.requestMemoryReadWork(ctx, target, &requirement)
}

func (storage *PostgresStore) requestMemoryReadWork(ctx context.Context, target domain.MemoryReadTarget, requirement *domain.MemoryReadRequirement) (work domain.MemoryReadWork, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	raw, err := domain.MemoryReadTargetBytes(ctx, target)
	if err != nil {
		return work, err
	}
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return work, err
	}
	if requirement != nil {
		if err := requirement.Validate(target); err != nil {
			return work, err
		}
	}
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return work, err
	}
	defer rollbackPG(transaction)
	if err := lockMemoryReadWorkScope(ctx, transaction, target.RepoID, true); err != nil {
		return work, err
	}
	source, err := readMemoryQualificationSource(ctx, transaction, target.RepoID)
	if err != nil {
		return work, err
	}
	if requirement != nil {
		if err := domain.CheckMemorySourceMinimum(source, requirement.Minimum); err != nil {
			if errors.Is(err, domain.ErrMemoryReadNotReady) {
				return work, domain.ErrValidation
			}
			return work, err
		}
	}
	var snapshot string
	if err := transaction.QueryRow(ctx, `SELECT id FROM snapshots WHERE repo_id=$1 AND id=$2 FOR SHARE NOWAIT`,
		string(target.RepoID), string(target.Selection.SnapshotID)).Scan(&snapshot); err != nil {
		return work, mapNoRows(err)
	}
	prior, err := scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2 FOR UPDATE`, string(target.RepoID), string(identifier)), target.RepoID, identifier)
	exists := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return work, err
	}
	active := exists && (prior.State == "waiting" || prior.State == "running" || prior.State == "retrying")
	coversPrior := requirement != nil && exists && domain.CheckMemorySourceMinimum(source, prior.Source) == nil
	if active && coversPrior {
		return prior, transaction.Commit(ctx)
	}
	if exists && (prior.Source == source || (coversPrior && prior.State == "completed")) {
		if requirement != nil && prior.State == "rejected" {
			return work, domain.ErrMemoryReadNeedsAttention
		}
		ready := prior.State != "completed"
		if !ready && prior.QualificationID != "" {
			var err error
			if requirement != nil {
				err = minimumMemoryReadDemandReady(ctx, transaction, target, *requirement, source, prior.QualificationID)
			} else {
				bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true, repo: target.RepoID})
				var header domain.MemoryReadHeader
				header, _, err = storage.currentMemoryReadInTx(bound, transaction, target, prior.QualificationID)
				if err == nil {
					raw, encodeErr := domain.MemoryReadHeaderBytes(ctx, header)
					if encodeErr != nil {
						return work, encodeErr
					}
					err = requireMemoryReadComparison(ctx, transaction, domain.HashContent(raw), identifier, header, false)
				}
			}
			if errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
				return work, domain.ErrMemoryReadNeedsAttention
			}
			if err != nil && !memoryReadWorkUnavailable(err) && !errors.Is(err, domain.ErrMemoryReadNotReady) {
				return work, err
			}
			ready = err == nil
		}
		if ready {
			return prior, transaction.Commit(ctx)
		}
	}
	if !active {
		var pending int
		if err := transaction.QueryRow(ctx, memoryReadWorkActiveCountSQL, string(target.RepoID), domain.MaxPendingMemoryReadWork).Scan(&pending); err != nil {
			return work, err
		}
		if pending >= domain.MaxPendingMemoryReadWork {
			return work, domain.ErrResourceOverloaded
		}
	}
	if !exists {
		var retained int
		if err := transaction.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM memory_read_work WHERE repo_id=$1 LIMIT $2) retained`,
			string(target.RepoID), domain.MaxMemoryReadTargets).Scan(&retained); err != nil {
			return work, err
		}
		if retained >= domain.MaxMemoryReadTargets {
			removed, err := transaction.Exec(ctx, `DELETE FROM memory_read_work WHERE repo_id=$1 AND target_id=(
 SELECT target_id FROM memory_read_work WHERE repo_id=$1 AND state IN ('completed','rejected')
 ORDER BY updated_at,target_id LIMIT 1 FOR UPDATE)`, string(target.RepoID))
			if err != nil {
				return work, err
			}
			if removed.RowsAffected() != 1 {
				return work, domain.ErrResourceOverloaded
			}
		}
	}
	sourceRaw, err := json.Marshal(source)
	if err != nil {
		return work, err
	}
	work, err = scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `INSERT INTO memory_read_work(repo_id,target_id,target,source,state)
 VALUES($1,$2,$3,$4,'waiting') ON CONFLICT(repo_id,target_id) DO UPDATE SET source=EXCLUDED.source,state='waiting',reason='',
 attempts=0,version=EXCLUDED.version,lease_until=NULL,updated_at=clock_timestamp(),next_attempt=clock_timestamp()
 RETURNING `+memoryReadWorkColumns, string(target.RepoID), string(identifier), raw, sourceRaw), target.RepoID, identifier)
	if err != nil {
		return work, err
	}
	return work, transaction.Commit(ctx)
}

func minimumMemoryReadDemandReady(ctx context.Context, transaction pgx.Tx, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement, source domain.RepositoryMemorySourceGeneration, identifier domain.ContentHash) error {
	_, qualification, err := readMemoryQualification(ctx, transaction, target.RepoID, identifier)
	if err != nil {
		return err
	}
	header, _, err := readMemoryReadDescriptor(ctx, transaction, target.RepoID, qualification.ReadID)
	if err != nil {
		return err
	}
	if err := domain.ValidateMemoryReadQualification(ctx, qualification, header); err != nil {
		return err
	}
	if header.Source.Graph.RepoID != target.RepoID || header.Rules != target.Rules || header.View.Selection != target.Selection || header.View.Content != target.Content {
		return domain.ErrIntegrity
	}
	if err := requireMemoryReadComparison(ctx, transaction, qualification.ReadID, requirement.TargetID, header, false); err != nil {
		return err
	}
	if err := domain.CheckMemorySourceMinimum(source, qualification.Source); err != nil {
		return err
	}
	if err := domain.CheckMemorySourceMinimum(qualification.Source, requirement.Minimum); err != nil {
		return err
	}
	if err := validateMinimumMemoryObjectWitnesses(ctx, transaction, target.RepoID, qualification.Witnesses); err != nil {
		return err
	}
	return memoryReadCheckExpiry(ctx, transaction, target.RepoID, qualification.CaptureID)
}

func (storage *PostgresStore) ReadMemoryReadWork(ctx context.Context, repo, identifier domain.ContentHash) (domain.MemoryReadWork, error) {
	if err := validateHashes(repo, identifier); err != nil {
		return domain.MemoryReadWork{}, err
	}
	transaction, owned, err := storage.memoryReadSnapshot(ctx, repo, identifier)
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	if owned {
		defer rollbackPG(transaction)
	}
	work, err := scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(repo), string(identifier)), repo, identifier)
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	if owned {
		err = transaction.Commit(ctx)
	}
	return work, err
}
