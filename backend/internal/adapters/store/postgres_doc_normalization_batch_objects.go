//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const legacyNormalizationBatchObjectRead = `WITH verified AS MATERIALIZED (
 SELECT octet_length(bytes) AS stored_bytes,CASE WHEN octet_length(bytes)=$4
 THEN 'sha256:'||encode(sha256(bytes),'hex') ELSE '' END AS stored_hash
 FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3
), touched AS (
 INSERT INTO doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,hash,last_sequence)
 SELECT $1,$2::uuid,$3,$5 FROM verified WHERE stored_bytes=$4 AND stored_hash=$3
 ON CONFLICT(doc_hash,stage_id,hash) DO UPDATE
 SET last_sequence=GREATEST(doc_legacy_checkpoint_staging_v1.last_sequence,EXCLUDED.last_sequence)
 RETURNING 1
)
 SELECT stored_bytes,stored_hash,EXISTS(SELECT 1 FROM touched) FROM verified`

func (batch *legacyNormalizationCheckpointBatch) verify(ctx context.Context, objects []legacyByteCheckpointOwnedObject, allowMissing bool) (missing []legacyByteCheckpointOwnedObject, failure error) {
	queries := &pgx.Batch{}
	for _, object := range objects {
		queries.Queue(legacyNormalizationBatchObjectRead, batch.owner.job.DocHash, batch.state.stage.incarnation,
			object.hash, len(object.data), batch.state.sequence)
	}
	results := batch.transaction.SendBatch(ctx, queries)
	defer func() { failure = errors.Join(failure, results.Close()) }()
	for _, object := range objects {
		var storedBytes int
		var storedHash domain.ContentHash
		var touched bool
		err := results.QueryRow().Scan(&storedBytes, &storedHash, &touched)
		if errors.Is(err, pgx.ErrNoRows) && allowMissing {
			missing = append(missing, object)
			continue
		}
		if err != nil {
			return nil, err
		}
		if storedBytes != len(object.data) || storedHash != object.hash || !touched {
			return nil, domain.ErrPreparedDocRangeObject
		}
	}
	return missing, ctx.Err()
}

func (batch *legacyNormalizationCheckpointBatch) flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batch.transaction == nil || batch.state != batch.owner.state || batch.state.stage.plan != "" ||
		len(batch.objects) > domain.DocLegacyByteBackfillBatchBlocks || batch.bytes < 0 || batch.bytes > domain.LegacyCanonicalEncodingBytes {
		return domain.ErrConflict
	}
	if len(batch.objects) == 0 {
		return nil
	}
	missing, err := batch.verify(ctx, batch.objects, true)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		inserts := &pgx.Batch{}
		for _, object := range missing {
			inserts.Queue(`INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash,bytes)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`,
				batch.owner.job.DocHash, batch.state.stage.incarnation, object.hash, object.data)
		}
		if err := batch.transaction.SendBatch(ctx, inserts).Close(); err != nil {
			return err
		}
		if _, err := batch.verify(ctx, missing, false); err != nil {
			return err
		}
	}
	structures := &pgx.Batch{}
	for _, object := range batch.objects {
		if object.structure != 0 {
			structures.Queue(`INSERT INTO doc_legacy_checkpoint_structures_v1(doc_hash,stage_id,hash,kind)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`,
				batch.owner.job.DocHash, batch.state.stage.incarnation, object.hash, object.structure)
		}
	}
	if structures.Len() != 0 {
		if err := batch.transaction.SendBatch(ctx, structures).Close(); err != nil {
			return err
		}
		if err := batch.verifyStructures(ctx); err != nil {
			return err
		}
	}
	clear(batch.objects)
	batch.objects = batch.objects[:0]
	batch.bytes = 0
	return ctx.Err()
}

func (batch *legacyNormalizationCheckpointBatch) verifyStructures(ctx context.Context) (failure error) {
	queries := &pgx.Batch{}
	for _, object := range batch.objects {
		if object.structure != 0 {
			queries.Queue(`SELECT kind FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`,
				batch.owner.job.DocHash, batch.state.stage.incarnation, object.hash)
		}
	}
	results := batch.transaction.SendBatch(ctx, queries)
	defer func() { failure = errors.Join(failure, results.Close()) }()
	for _, object := range batch.objects {
		if object.structure == 0 {
			continue
		}
		var kind uint8
		if err := results.QueryRow().Scan(&kind); err != nil {
			return err
		}
		if kind != object.structure {
			return domain.ErrPreparedDocRangeObject
		}
	}
	return ctx.Err()
}
