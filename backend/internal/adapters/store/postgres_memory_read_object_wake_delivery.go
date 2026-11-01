//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const memoryReadObjectWakeDeliveryUpsertSQL = `INSERT INTO memory_read_object_wake_deliveries(hash,repo_id,version)
 SELECT $1,repo_id,$2 FROM unnest($3::text[]) recipients(repo_id) ORDER BY repo_id COLLATE "C"
 ON CONFLICT(hash,repo_id) DO UPDATE SET version=EXCLUDED.version,next_attempt=LEAST(memory_read_object_wake_deliveries.next_attempt,clock_timestamp()),attempts=0,reason=''
 WHERE memory_read_object_wake_deliveries.version<EXCLUDED.version`

func (storage *PostgresStore) deliverMemoryReadObjectWake(ctx context.Context) (failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='5ms'; SET LOCAL statement_timeout='100ms'`); err != nil {
		return err
	}
	var hash, repo string
	var version int64
	if err := transaction.QueryRow(ctx, `SELECT hash,repo_id,version FROM memory_read_object_wake_deliveries
 WHERE next_attempt<=clock_timestamp() ORDER BY next_attempt,hash,repo_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&hash, &repo, &version); err != nil {
		return mapNoRows(err)
	}
	delivery, err := transaction.Begin(ctx)
	if err != nil {
		return err
	}
	if err := deliverMemoryReadObjectWakeRecipient(ctx, delivery, hash, repo, version); err != nil {
		if rollbackErr := delivery.Rollback(ctx); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := "temporary_failure"
		if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) {
			reason = "invalid_source"
		}
		tag, retryErr := transaction.Exec(ctx, `UPDATE memory_read_object_wake_deliveries SET
 attempts=LEAST(attempts+1,6),reason=$4,
 next_attempt=clock_timestamp()+power(2,LEAST(attempts,5))*interval '1 second'
 WHERE hash=$1 AND repo_id=$2 AND version=$3`, hash, repo, version, reason)
		if retryErr != nil {
			return retryErr
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else if err := delivery.Commit(ctx); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func deliverMemoryReadObjectWakeRecipient(ctx context.Context, transaction pgx.Tx, hash, repo string, version int64) error {
	if err := domain.ValidateContentHash(domain.ContentHash(hash)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return err
	}
	var present int
	if err := transaction.QueryRow(ctx, `SELECT 1 FROM memory_read_object_wake_deliveries
 WHERE hash=$1 AND repo_id=$2 AND version=$3 FOR UPDATE`, hash, repo, version).Scan(&present); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		return err
	}
	var enabled bool
	err := transaction.QueryRow(ctx, `SELECT enabled FROM memory_read_work_scopes WHERE repo_id=$1 FOR SHARE NOWAIT`, repo).Scan(&enabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if enabled {
		var kind string
		err = transaction.QueryRow(ctx, `SELECT kind FROM repo_blobs
 WHERE repo_id=$1 AND hash=$2 AND kind IN ('memory','memory_chunk') ORDER BY kind LIMIT 1 FOR SHARE NOWAIT`, repo, hash).Scan(&kind)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			err = transaction.QueryRow(ctx, `SELECT 1 FROM memory_read_wakes WHERE repo_id=$1 FOR UPDATE NOWAIT`, repo).Scan(&present)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := transaction.Exec(ctx, `SELECT cxt_notify_memory_read_wake($1,true)`, repo); err != nil {
				return err
			}
		}
	}
	tag, err := transaction.Exec(ctx, `DELETE FROM memory_read_object_wake_deliveries WHERE hash=$1 AND repo_id=$2 AND version=$3`, hash, repo, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
