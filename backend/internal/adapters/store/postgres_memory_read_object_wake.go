//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const memoryReadObjectWakeBatch = 32

var _ outbound.MemoryReadObjectWakeDispatcher = (*PostgresStore)(nil)

const memoryReadObjectWakeOwnersSQL = `SELECT DISTINCT owned.repo_id COLLATE "C" AS repo_id
 FROM repo_blobs owned JOIN memory_read_work_scopes scope ON scope.repo_id=owned.repo_id AND scope.enabled
 WHERE owned.hash=$1 AND owned.kind IN ('memory','memory_chunk')
 AND owned.repo_id COLLATE "C">$2 AND owned.repo_id COLLATE "C"<=$3
 ORDER BY repo_id LIMIT 33`

const memoryReadObjectWakeThroughSQL = `SELECT owned.repo_id FROM repo_blobs owned
 JOIN memory_read_work_scopes scope ON scope.repo_id=owned.repo_id AND scope.enabled
 WHERE owned.hash=$1 AND owned.kind IN ('memory','memory_chunk')
 ORDER BY owned.repo_id COLLATE "C" DESC LIMIT 1`

func (storage *PostgresStore) DispatchMemoryReadObjectWakes(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > memoryReadObjectWakeBatch {
		return 0, domain.ErrValidation
	}
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return 0, err
	}
	if outbound.DocReadOnly(ctx) {
		return 0, domain.ErrConflict
	}
	processed := 0
	if _, err := storage.dispatchMemoryReadObjectWake(ctx); err == nil {
		processed++
	} else if !errors.Is(err, domain.ErrNotFound) {
		return processed, err
	}
	for range limit {
		err := storage.deliverMemoryReadObjectWake(ctx)
		if errors.Is(err, domain.ErrNotFound) {
			return processed, nil
		}
		if err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (storage *PostgresStore) dispatchMemoryReadObjectWake(ctx context.Context) (hash string, failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return hash, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='50ms'; SET LOCAL statement_timeout='100ms'`); err != nil {
		return hash, err
	}
	var version, scanVersion int64
	var afterRepo string
	if err := transaction.QueryRow(ctx, `SELECT hash,version,scan_version,after_repo FROM memory_read_object_wakes
 WHERE next_attempt<=clock_timestamp()
 ORDER BY next_attempt,hash FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&hash, &version, &scanVersion, &afterRepo); err != nil {
		return hash, mapNoRows(err)
	}
	page, err := transaction.Begin(ctx)
	if err != nil {
		return hash, err
	}
	err = dispatchMemoryReadObjectWakePage(ctx, page, hash, version, scanVersion, afterRepo)
	if err != nil {
		if rollbackErr := page.Rollback(ctx); rollbackErr != nil {
			return hash, errors.Join(err, rollbackErr)
		}
		if ctx.Err() != nil {
			return hash, ctx.Err()
		}
		reason := "temporary_failure"
		if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) {
			reason = "invalid_source"
		}
		tag, retryErr := transaction.Exec(ctx, `UPDATE memory_read_object_wakes SET
 attempts=LEAST(attempts+1,6),reason=$3,
 next_attempt=clock_timestamp()+power(2,LEAST(attempts,5))*interval '1 second'
 WHERE hash=$1 AND version=$2`, hash, version, reason)
		if retryErr != nil {
			return hash, retryErr
		}
		if tag.RowsAffected() != 1 {
			return hash, domain.ErrConflict
		}
	} else if err := page.Commit(ctx); err != nil {
		return hash, err
	}
	return hash, transaction.Commit(ctx)
}

func dispatchMemoryReadObjectWakePage(ctx context.Context, transaction pgx.Tx, hash string, version, scanVersion int64, afterRepo string) error {
	var throughRepo pgtype.Text
	if err := transaction.QueryRow(ctx, `SELECT through_repo FROM memory_read_object_wakes
 WHERE hash=$1 AND version=$2 AND after_repo=$3 AND scan_version=$4 FOR UPDATE`, hash, version, afterRepo, scanVersion).Scan(&throughRepo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		return err
	}
	if scanVersion == 0 {
		scanVersion = version
	}
	if !throughRepo.Valid {
		throughRepo.Valid = true
		err := transaction.QueryRow(ctx, memoryReadObjectWakeThroughSQL, hash).Scan(&throughRepo.String)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	rows, err := transaction.Query(ctx, memoryReadObjectWakeOwnersSQL, hash, afterRepo, throughRepo.String)
	if err != nil {
		return err
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	more := len(recipients) > memoryReadObjectWakeBatch
	if more {
		recipients = recipients[:memoryReadObjectWakeBatch]
	}
	for _, repo := range recipients {
		if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
			return err
		}
	}
	if len(recipients) > 0 {
		if _, err := transaction.Exec(ctx, memoryReadObjectWakeDeliveryUpsertSQL, hash, scanVersion, recipients); err != nil {
			return err
		}
	}
	if more {
		tag, err := transaction.Exec(ctx, `UPDATE memory_read_object_wakes SET after_repo=$3,scan_version=$4,through_repo=$5,
 next_attempt=clock_timestamp(),attempts=0,reason='' WHERE hash=$1 AND version=$2`, hash, version, recipients[len(recipients)-1], scanVersion, throughRepo.String)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else if version > scanVersion {
		tag, err := transaction.Exec(ctx, `UPDATE memory_read_object_wakes SET after_repo='',scan_version=version,through_repo=NULL,
 next_attempt=clock_timestamp(),attempts=0,reason='' WHERE hash=$1 AND version=$2`, hash, version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else {
		tag, err := transaction.Exec(ctx, `DELETE FROM memory_read_object_wakes WHERE hash=$1 AND version=$2`, hash, version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	return nil
}
