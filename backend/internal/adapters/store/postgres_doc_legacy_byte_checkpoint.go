//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyByteCheckpoint struct {
	stage    legacyByteStage
	sequence int64
	hash     domain.ContentHash
	bytes    int
}

type legacyByteCheckpointObject struct {
	hash      domain.ContentHash
	data      []byte
	structure uint8
}

func legacyByteCheckpointProtocols(ctx context.Context, transaction pgx.Tx) error {
	_, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'; SET LOCAL cxt.legacy_byte_checkpoint_protocol='1'`)
	return err
}

func (storage *PostgresStore) openLegacyByteCheckpoint(ctx context.Context, job domain.DocRangeIndexJob, source legacyByteStage, expected *legacyByteCheckpoint, create bool) (_ pgx.Tx, result legacyByteCheckpoint, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, result, err
	}
	if job.Validate() != nil || job.State != "running" || job.BaseHash != "" || source.sourceVersion < 0 || create && source.sourceID == "" {
		return nil, result, domain.ErrValidation
	}
	if _, nested := ctx.Value(repositoryTxKey{}).(*repositoryTx); nested || outbound.DocReadOnly(ctx) {
		return nil, result, domain.ErrConflict
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return nil, result, err
	}
	retained := false
	defer func() {
		if !retained {
			rollbackPG(transaction)
		}
	}()
	if expected != nil && !create && source.sourceID != "" && source.incarnation != "" {
		var current legacyByteStage
		var locked bool
		err := transaction.QueryRow(ctx, `WITH retained_source AS MATERIALIZED (
 SELECT original.doc_range_source_version AS version,incarnation.incarnation::text AS id
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs original ON original.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=original.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,original,incarnation
), retained_stage AS MATERIALIZED (
 SELECT stage.source_id::text AS source_id,stage.source_version,COALESCE(stage.plan_hash,'') AS plan_hash,
 COALESCE(stage.blocks,0) AS blocks,stage.next_block,stage.complete,stage.stage_id::text AS stage_id,
 stage.checkpoint_sequence,COALESCE(stage.checkpoint_hash,'') AS checkpoint_hash,stage.checkpoint_bytes
 FROM doc_legacy_byte_stages_v1 stage CROSS JOIN retained_source source
 WHERE stage.doc_hash=$2 AND source.version=$3 AND source.id=$4 FOR UPDATE OF stage
), retained_job AS MATERIALIZED (
 SELECT 1 FROM doc_range_jobs_v1 job CROSS JOIN retained_stage stage CROSS JOIN retained_source source
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash=$9 AND stage.source_id=source.id
 AND stage.source_version=source.version AND stage.stage_id=$5 AND stage.checkpoint_sequence=$6
 AND stage.checkpoint_hash=$7 AND stage.checkpoint_bytes=$8 FOR UPDATE OF job
)
 SELECT source.version,source.id,stage.source_id,stage.source_version,stage.plan_hash,stage.blocks,
 stage.next_block,stage.complete,stage.stage_id,stage.checkpoint_sequence,stage.checkpoint_hash,
 stage.checkpoint_bytes,EXISTS(SELECT 1 FROM retained_job) FROM retained_source source CROSS JOIN retained_stage stage`,
			job.RepoID, job.DocHash, source.sourceVersion, source.sourceID, expected.stage.incarnation,
			expected.sequence, expected.hash, expected.bytes, job.BaseHash).
			Scan(&current.sourceVersion, &current.sourceID, &result.stage.sourceID, &result.stage.sourceVersion,
				&result.stage.plan, &result.stage.blocks, &result.stage.next, &result.stage.complete,
				&result.stage.incarnation, &result.sequence, &result.hash, &result.bytes, &locked)
		if errors.Is(err, pgx.ErrNoRows) {
			rollbackPG(transaction)
			if current := storage.confirmLegacyBytePreparation(ctx, job, source); current != nil {
				return nil, result, current
			}
			return nil, result, domain.ErrConflict
		}
		if err != nil {
			return nil, result, err
		}
		if current.sourceVersion != source.sourceVersion || current.sourceID != source.sourceID ||
			result.stage.sourceVersion != current.sourceVersion || result.stage.sourceID != current.sourceID ||
			expected.stage.incarnation == "" || result.stage.incarnation != source.incarnation ||
			result.stage.incarnation != expected.stage.incarnation || result.sequence != expected.sequence ||
			result.hash != expected.hash || result.bytes != expected.bytes || !locked {
			return nil, result, domain.ErrConflict
		}
		if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
			return nil, result, err
		}
		retained = true
		return transaction, result, nil
	}
	var current legacyByteStage
	err = transaction.QueryRow(ctx, `SELECT original.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs original ON original.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=original.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,original,incarnation`, job.RepoID, job.DocHash).Scan(&current.sourceVersion, &current.sourceID)
	if err != nil {
		return nil, result, mapNoRows(err)
	}
	if source.sourceID != "" && (source.sourceID != current.sourceID || source.sourceVersion != current.sourceVersion) {
		return nil, result, domain.ErrConflict
	}
	readStage := func() error {
		return transaction.QueryRow(ctx, `SELECT source_id::text,source_version,COALESCE(plan_hash,''),COALESCE(blocks,0),next_block,complete,
 stage_id::text,checkpoint_sequence,COALESCE(checkpoint_hash,''),checkpoint_bytes
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR UPDATE`, job.DocHash).
			Scan(&result.stage.sourceID, &result.stage.sourceVersion, &result.stage.plan, &result.stage.blocks, &result.stage.next,
				&result.stage.complete, &result.stage.incarnation, &result.sequence, &result.hash, &result.bytes)
	}
	err = readStage()
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, result, err
	}
	present := err == nil
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return nil, result, err
	}
	if present && (result.stage.sourceID != current.sourceID || result.stage.sourceVersion != current.sourceVersion) {
		if expected != nil || source.incarnation != "" {
			return nil, result, domain.ErrConflict
		}
		if create {
			if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash); err != nil {
				return nil, result, err
			}
		}
		present, result = false, legacyByteCheckpoint{}
	}
	if !present {
		if expected != nil || source.incarnation != "" {
			return nil, result, domain.ErrConflict
		}
		if create {
			if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
				return nil, result, err
			}
			result.stage = current
			err := transaction.QueryRow(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete)
 VALUES($1,$2::uuid,$3,NULL,NULL,NULL,0,false) ON CONFLICT(doc_hash) DO NOTHING RETURNING stage_id::text`, job.DocHash, current.sourceID, current.sourceVersion).
				Scan(&result.stage.incarnation)
			if errors.Is(err, pgx.ErrNoRows) {
				if err := readStage(); err != nil {
					return nil, result, err
				}
				if result.stage.sourceID != current.sourceID || result.stage.sourceVersion != current.sourceVersion {
					return nil, result, domain.ErrConflict
				}
				present = true
			} else if err != nil {
				return nil, result, err
			}
		}
	}
	if present && source.incarnation != "" && result.stage.incarnation != source.incarnation {
		return nil, result, domain.ErrConflict
	}
	if expected != nil && (expected.stage.incarnation == "" || result.stage.incarnation != expected.stage.incarnation ||
		result.sequence != expected.sequence || result.hash != expected.hash || result.bytes != expected.bytes) {
		return nil, result, domain.ErrConflict
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return nil, result, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return nil, result, err
	}
	retained = true
	return transaction, result, nil
}

func (storage *PostgresStore) ensureLegacyNormalizationStage(ctx context.Context, job domain.DocRangeIndexJob, source legacyByteStage) (legacyByteStage, error) {
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, source, nil, true)
	if err != nil {
		return legacyByteStage{}, err
	}
	defer rollbackPG(transaction)
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteStage{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyByteStage{}, err
	}
	return state.stage, nil
}

func (storage *PostgresStore) loadLegacyNormalizationCheckpoint(ctx context.Context, job domain.DocRangeIndexJob) (legacyByteCheckpoint, bool, error) {
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, legacyByteStage{}, nil, false)
	if err != nil {
		return legacyByteCheckpoint{}, false, err
	}
	defer rollbackPG(transaction)
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteCheckpoint{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyByteCheckpoint{}, false, err
	}
	return state, state.hash != "", nil
}

type legacyByteCheckpointOwnedObject legacyByteCheckpointObject

func legacyByteCheckpointObjects(ctx context.Context, objects []legacyByteCheckpointObject) ([]legacyByteCheckpointOwnedObject, error) {
	if len(objects) > domain.DocLegacyByteBackfillBatchBlocks {
		return nil, domain.ErrValidation
	}
	owned := make([]legacyByteCheckpointOwnedObject, 0, len(objects))
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(object.data) == 0 || len(object.data) > domain.LegacyCanonicalEncodingBytes || domain.ValidateContentHash(object.hash) != nil || object.structure > 2 {
			return nil, domain.ErrValidation
		}
		data := bytes.Clone(object.data)
		hash, err := domain.HashContentContext(ctx, data)
		if err != nil || hash != object.hash {
			return nil, errors.Join(domain.ErrIntegrity, err)
		}
		owned = append(owned, legacyByteCheckpointOwnedObject{hash: hash, data: data, structure: object.structure})
	}
	return owned, nil
}

func putLegacyByteCheckpointObjects(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, state legacyByteCheckpoint, objects []legacyByteCheckpointOwnedObject) error {
	if state.stage.plan != "" {
		return domain.ErrConflict
	}
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		return err
	}
	if err := writeLegacyByteCheckpointObjects(ctx, transaction, job, state, objects); err != nil {
		return err
	}
	return liveLegacyByteJob(ctx, transaction, job)
}

func writeLegacyByteCheckpointObjects(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, state legacyByteCheckpoint, objects []legacyByteCheckpointOwnedObject) error {
	if state.stage.plan != "" {
		return domain.ErrConflict
	}
	for _, object := range objects {
		var storedBytes int
		var storedHash domain.ContentHash
		var touched bool
		read := func() error {
			return transaction.QueryRow(ctx, legacyNormalizationBatchObjectRead,
				job.DocHash, state.stage.incarnation, object.hash, len(object.data), state.sequence).Scan(&storedBytes, &storedHash, &touched)
		}
		err := read()
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash,bytes)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`, job.DocHash, state.stage.incarnation, object.hash, object.data); err != nil {
				return err
			}
			err = read()
		}
		if err != nil {
			return err
		}
		if storedBytes != len(object.data) || storedHash != object.hash || !touched {
			return domain.ErrPreparedDocRangeObject
		}
		if object.structure != 0 {
			if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_checkpoint_structures_v1(doc_hash,stage_id,hash,kind)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`, job.DocHash, state.stage.incarnation, object.hash, object.structure); err != nil {
				return err
			}
			var kind uint8
			if err := transaction.QueryRow(ctx, `SELECT kind FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`,
				job.DocHash, state.stage.incarnation, object.hash).Scan(&kind); err != nil {
				return err
			}
			if kind != object.structure {
				return domain.ErrPreparedDocRangeObject
			}
		}
	}
	return ctx.Err()
}

func (storage *PostgresStore) stageLegacyNormalizationCheckpointObjects(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, objects []legacyByteCheckpointObject) error {
	owned, err := legacyByteCheckpointObjects(ctx, objects)
	if err != nil {
		return err
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := putLegacyByteCheckpointObjects(ctx, transaction, job, state, owned); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) saveLegacyNormalizationCheckpoint(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, root domain.ContentHash, rootBytes int, objects []legacyByteCheckpointObject) (legacyByteCheckpoint, error) {
	if expected.sequence < 0 || expected.sequence == math.MaxInt64 || domain.ValidateContentHash(root) != nil || rootBytes < 1 || rootBytes > domain.LegacyCanonicalEncodingBytes {
		return legacyByteCheckpoint{}, domain.ErrValidation
	}
	owned, err := legacyByteCheckpointObjects(ctx, objects)
	if err != nil {
		return legacyByteCheckpoint{}, err
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return legacyByteCheckpoint{}, err
	}
	defer rollbackPG(transaction)
	if err := putLegacyByteCheckpointObjects(ctx, transaction, job, state, owned); err != nil {
		return legacyByteCheckpoint{}, err
	}
	var stored []byte
	if err := transaction.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $4) FROM doc_legacy_byte_stage_checkpoint_objects_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, root, rootBytes+1).Scan(&stored); err != nil {
		return legacyByteCheckpoint{}, errors.Join(domain.ErrPreparedDocRangeObject, mapNoRows(err))
	}
	actual, err := domain.HashContentContext(ctx, stored)
	if err != nil || len(stored) != rootBytes || actual != root {
		return legacyByteCheckpoint{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	if err := transaction.QueryRow(ctx, `UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=checkpoint_sequence+1,
 checkpoint_hash=$2,checkpoint_bytes=$3 WHERE doc_hash=$1 AND stage_id=$4::uuid AND checkpoint_sequence=$5
 AND checkpoint_hash IS NOT DISTINCT FROM NULLIF($6,'') RETURNING checkpoint_sequence`,
		job.DocHash, root, rootBytes, expected.stage.incarnation, expected.sequence, expected.hash).Scan(&state.sequence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return legacyByteCheckpoint{}, domain.ErrConflict
		}
		return legacyByteCheckpoint{}, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteCheckpoint{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyByteCheckpoint{}, err
	}
	state.hash, state.bytes = root, rootBytes
	return state, nil
}

func (storage *PostgresStore) readLegacyNormalizationCheckpointObject(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, hash domain.ContentHash, size int) ([]byte, error) {
	if domain.ValidateContentHash(hash) != nil || size < 1 || size > domain.LegacyCanonicalEncodingBytes {
		return nil, domain.ErrValidation
	}
	transaction, _, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return nil, err
	}
	defer rollbackPG(transaction)
	data, err := readLegacyNormalizationCheckpointObject(ctx, transaction, job, expected, hash, size)
	if err != nil {
		return nil, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func readLegacyNormalizationCheckpointObject(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, hash domain.ContentHash, size int) ([]byte, error) {
	if domain.ValidateContentHash(hash) != nil || size < 1 || size > domain.LegacyCanonicalEncodingBytes {
		return nil, domain.ErrValidation
	}
	var data []byte
	if err := transaction.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $4) FROM doc_legacy_byte_stage_checkpoint_objects_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, expected.stage.incarnation, hash, size+1).Scan(&data); err != nil {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, mapNoRows(err))
	}
	actual, err := domain.HashContentContext(ctx, data)
	if err != nil || len(data) != size || actual != hash {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return data, nil
}

func (storage *PostgresStore) discardLegacyNormalizationCheckpoint(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint) error {
	if expected.hash == "" || expected.sequence == math.MaxInt64 {
		return domain.ErrValidation
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		return err
	}
	if err := transaction.QueryRow(ctx, `UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=checkpoint_sequence+1,
 checkpoint_hash=NULL,checkpoint_bytes=0 WHERE doc_hash=$1 AND stage_id=$2::uuid AND checkpoint_sequence=$3
 AND checkpoint_hash=$4 RETURNING checkpoint_sequence`, job.DocHash, expected.stage.incarnation, expected.sequence, expected.hash).Scan(&state.sequence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		return err
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid`, job.DocHash, state.stage.incarnation); err != nil {
		return err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}
