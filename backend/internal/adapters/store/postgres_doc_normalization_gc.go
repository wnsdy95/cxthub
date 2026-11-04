//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func touchLegacyCheckpointObject(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, state legacyByteCheckpoint, hash domain.ContentHash, size int) error {
	var present bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_checkpoint_objects_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3 AND octet_length(bytes)=$4)`,
		job.DocHash, state.stage.incarnation, hash, size).Scan(&present); err != nil {
		return err
	}
	if !present {
		return domain.ErrPreparedDocRangeObject
	}
	_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,hash,last_sequence)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO UPDATE
 SET last_sequence=GREATEST(doc_legacy_checkpoint_staging_v1.last_sequence,EXCLUDED.last_sequence)`,
		job.DocHash, state.stage.incarnation, hash, state.sequence)
	return err
}

func (storage *PostgresStore) touchLegacyNormalizationPrefix(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, tree legacyNormalizationPrefixTree) error {
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if state.stage.plan != "" {
		return domain.ErrConflict
	}
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		return err
	}
	if tree.Leaves != 0 {
		if err := touchLegacyCheckpointObject(ctx, transaction, job, state, tree.Hash, tree.Bytes); err != nil {
			return err
		}
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) reclaimLegacyNormalizationCheckpoint(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint) (bool, int64, error) {
	if expected.hash == "" {
		return false, 0, domain.ErrDocRangeIndexPending
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return false, 0, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'; SET LOCAL cxt.legacy_checkpoint_gc_protocol='1'; SET LOCAL enable_seqscan=off`); err != nil {
		return false, 0, err
	}
	var kind uint8
	if err := transaction.QueryRow(ctx, `SELECT kind FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`,
		job.DocHash, state.stage.incarnation, state.hash).Scan(&kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, domain.ErrDocRangeIndexPending
		}
		return false, 0, err
	}
	if kind != 2 {
		return false, 0, domain.ErrPreparedDocRangeObject
	}
	var sequence, lastSequence int64
	var lastHash domain.ContentHash
	var changed, complete bool
	if err := transaction.QueryRow(ctx, `SELECT checkpoint_gc_sequence,checkpoint_gc_last_sequence,checkpoint_gc_last_hash,checkpoint_gc_changed,checkpoint_gc_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid`, job.DocHash, state.stage.incarnation).
		Scan(&sequence, &lastSequence, &lastHash, &changed, &complete); err != nil {
		return false, 0, err
	}
	if sequence != state.sequence {
		sequence, lastSequence, lastHash, changed, complete = state.sequence, -1, "", false, false
	}
	var removed int64
	if !complete {
		rows, err := transaction.Query(ctx, `SELECT last_sequence,hash FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND last_sequence<$3 AND (last_sequence,hash)>($4,$5)
 ORDER BY last_sequence,hash LIMIT 128`, job.DocHash, state.stage.incarnation, state.sequence, lastSequence, lastHash)
		if err != nil {
			return false, 0, err
		}
		candidates := make([]domain.ContentHash, 0, 128)
		for rows.Next() {
			if err := rows.Scan(&lastSequence, &lastHash); err != nil {
				rows.Close()
				return false, 0, err
			}
			candidates = append(candidates, lastHash)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, 0, err
		}
		if len(candidates) == 0 {
			if changed {
				lastSequence, lastHash, changed = -1, "", false
			} else {
				complete = true
			}
		} else {
			result, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 object
 WHERE object.doc_hash=$1 AND object.stage_id=$2::uuid AND object.hash=ANY($3::text[]) AND object.hash<>$4
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_checkpoint_edges_v1 edge
 WHERE edge.doc_hash=object.doc_hash AND edge.stage_id=object.stage_id AND edge.child_hash=object.hash)`,
				job.DocHash, state.stage.incarnation, candidates, state.hash)
			if err != nil {
				return false, 0, err
			}
			removed = result.RowsAffected()
			changed = changed || removed > 0
		}
	}
	result, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET checkpoint_gc_sequence=$3,checkpoint_gc_last_sequence=$4,
 checkpoint_gc_last_hash=$5,checkpoint_gc_changed=$6,checkpoint_gc_complete=$7
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND checkpoint_sequence=$3 AND checkpoint_hash=$8`,
		job.DocHash, state.stage.incarnation, sequence, lastSequence, lastHash, changed, complete, state.hash)
	if err != nil {
		return false, 0, err
	}
	if result.RowsAffected() != 1 {
		return false, 0, domain.ErrConflict
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return false, 0, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, 0, err
	}
	return complete, removed, nil
}
