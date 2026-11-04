//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const legacyByteRecoveryHeaderPartBytes = 64 << 10
const legacyByteRecoveryFrameBytes = len(`{"envelope":`) + len(`,"events":[`) + len(`]}`)

type legacyByteRecoveryState struct {
	stage       legacyByteStage
	encoded     []byte
	headerBytes int
}

func (storage *PostgresStore) readLegacyByteRecovery(ctx context.Context, job domain.DocRangeIndexJob) (legacyByteRecoveryState, bool, error) {
	if err := ctx.Err(); err != nil {
		return legacyByteRecoveryState{}, false, err
	}
	if err := job.Validate(); err != nil {
		return legacyByteRecoveryState{}, false, err
	}
	if job.State != "running" || job.BaseHash != "" {
		return legacyByteRecoveryState{}, false, domain.ErrValidation
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return legacyByteRecoveryState{}, false, err
	}
	defer rollbackPG(transaction)
	state, ready, err := readLegacyByteRecoveryMetadata(ctx, transaction, job, nil)
	if err := legacyByteRecoveryReadError(ctx, transaction, job, err); err != nil {
		return state, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return state, false, err
	}
	if err := ctx.Err(); err != nil {
		return state, false, err
	}
	return state, ready, nil
}

func (storage *PostgresStore) readLegacyByteRecoveryPart(ctx context.Context, job domain.DocRangeIndexJob, state legacyByteRecoveryState, header bool, ordinal int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if job.State != "running" || job.BaseHash != "" || ordinal < 0 {
		return nil, domain.ErrValidation
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return nil, err
	}
	defer rollbackPG(transaction)
	current, ready, err := readLegacyByteRecoveryMetadata(ctx, transaction, job, &state)
	var body []byte
	if err == nil {
		if !ready {
			err = domain.ErrConflict
		} else {
			body, err = readLegacyByteRecoveryPayload(ctx, transaction, job, current, header, ordinal)
		}
	}
	if err := legacyByteRecoveryReadError(ctx, transaction, job, err); err != nil {
		return nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func readLegacyByteRecoveryMetadata(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, expected *legacyByteRecoveryState) (legacyByteRecoveryState, bool, error) {
	var current legacyByteStage
	err := transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,source,incarnation`, job.RepoID, job.DocHash).Scan(&current.sourceVersion, &current.sourceID)
	if err != nil {
		return legacyByteRecoveryState{}, false, mapNoRows(err)
	}
	var state legacyByteRecoveryState
	var headerVersion, headerNext int
	var headerBytes *int
	var headerComplete bool
	stageErr := transaction.QueryRow(ctx, `SELECT source_id::text,source_version,COALESCE(substring(plan_hash FROM 1 FOR 72),''),
 substring(spec FROM 1 FOR $2),next_block,COALESCE(blocks,0),complete,stage_id::text,header_version,header_bytes,header_next,header_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR SHARE`, job.DocHash, domain.MaxDocLegacyByteBackfillSpecBytes+1).
		Scan(&state.stage.sourceID, &state.stage.sourceVersion, &state.stage.plan, &state.encoded,
			&state.stage.next, &state.stage.blocks, &state.stage.complete, &state.stage.incarnation, &headerVersion, &headerBytes, &headerNext, &headerComplete)
	if headerBytes != nil {
		state.headerBytes = *headerBytes
	}
	if stageErr != nil && !errors.Is(stageErr, pgx.ErrNoRows) {
		return state, false, stageErr
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return state, false, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return state, false, err
	}
	if errors.Is(stageErr, pgx.ErrNoRows) {
		if expected != nil {
			return state, false, domain.ErrConflict
		}
		return state, false, nil
	}
	if expected != nil && state.stage.incarnation != expected.stage.incarnation {
		return state, false, domain.ErrConflict
	}
	if state.stage.sourceID != current.sourceID || state.stage.sourceVersion != current.sourceVersion {
		if expected == nil {
			return state, false, nil
		}
		return state, false, domain.ErrConflict
	}
	if state.stage.plan == "" {
		if state.encoded != nil || state.stage.next != 0 || state.stage.blocks != 0 || state.stage.complete ||
			headerVersion != 0 || headerBytes != nil || headerNext != 0 || headerComplete {
			return state, false, domain.ErrPreparedDocRangeObject
		}
		if expected != nil {
			return state, false, domain.ErrConflict
		}
		return state, false, nil
	}
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(state.stage.plan, state.encoded)
	if err != nil || spec.Reference.Hash != job.DocHash || state.stage.blocks != spec.Blocks ||
		state.stage.next < 0 || state.stage.next > state.stage.blocks || state.stage.complete != (state.stage.next == state.stage.blocks) {
		return state, false, domain.ErrPreparedDocRangeObject
	}
	if headerVersion == 0 {
		if headerBytes != nil || headerNext != 0 || headerComplete {
			return state, false, domain.ErrPreparedDocRangeObject
		}
		return state, false, nil
	}
	if headerVersion != 1 || headerBytes == nil || state.headerBytes < 2 || state.headerBytes > domain.MaxConversationDocumentBytes ||
		spec.StreamBytes > int64(domain.MaxConversationDocumentBytes-legacyByteRecoveryFrameBytes-state.headerBytes) {
		return state, false, domain.ErrPreparedDocRangeObject
	}
	headerParts := (state.headerBytes + legacyByteRecoveryHeaderPartBytes - 1) / legacyByteRecoveryHeaderPartBytes
	if headerNext < 0 || headerNext > headerParts || headerComplete != (headerNext == headerParts) {
		return state, false, domain.ErrPreparedDocRangeObject
	}
	if expected != nil && (state.stage != expected.stage || state.headerBytes != expected.headerBytes || !bytes.Equal(state.encoded, expected.encoded)) {
		return state, false, domain.ErrConflict
	}
	if !state.stage.complete || !headerComplete {
		return state, false, nil
	}
	if expected == nil {
		if err := validateLegacyByteRecoveryExtent(ctx, transaction, job.DocHash, state, spec, headerParts); err != nil {
			return state, false, err
		}
	}
	return state, true, nil
}

func validateLegacyByteRecoveryExtent(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash, state legacyByteRecoveryState, spec domain.DocLegacyByteBackfillSpec, headerParts int) error {
	var valid bool
	err := transaction.QueryRow(ctx, `SELECT
 (SELECT count(*)=$2 AND COALESCE(bool_and(COALESCE(
 part.ordinal>=0 AND part.ordinal<$2
 AND part.byte_count=LEAST($3-part.ordinal::bigint*$4,$4::bigint)
 AND substring(part.hash FROM 1 FOR 72) ~ '^sha256:[0-9a-f]{64}$'
 AND octet_length(object.bytes)=part.byte_count,false)),true)
 FROM (SELECT ordinal,hash,byte_count FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1 LIMIT $2+1) part
 LEFT JOIN doc_legacy_byte_stage_objects_v1 object ON object.doc_hash=$1 AND object.hash=part.hash)
 AND
 (SELECT count(*)=$5 AND COALESCE(bool_and(COALESCE(
 header.ordinal>=0 AND header.ordinal<$5
 AND octet_length(header.bytes)=LEAST($6-header.ordinal::bigint*$7,$7::bigint)
 AND substring(header.hash FROM 1 FOR 72) ~ '^sha256:[0-9a-f]{64}$',false)),true)
 FROM (SELECT ordinal,hash,bytes FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 LIMIT $5+1) header)`,
		hash, state.stage.blocks, spec.StreamBytes, domain.ConversationManifestChunkBytes,
		headerParts, state.headerBytes, legacyByteRecoveryHeaderPartBytes).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.ErrPreparedDocRangeObject
	}
	return ctx.Err()
}

func readLegacyByteRecoveryPayload(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, state legacyByteRecoveryState, header bool, ordinal int) ([]byte, error) {
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(state.stage.plan, state.encoded)
	if err != nil {
		return nil, domain.ErrPreparedDocRangeObject
	}
	partBytes, extent := domain.ConversationManifestChunkBytes, int(spec.StreamBytes)
	if header {
		partBytes, extent = legacyByteRecoveryHeaderPartBytes, state.headerBytes
	}
	if ordinal < 0 || ordinal >= (extent+partBytes-1)/partBytes {
		return nil, domain.ErrValidation
	}
	length := min(partBytes, extent-ordinal*partBytes)
	statement := `SELECT substring(header.hash FROM 1 FOR 72),
 CASE WHEN octet_length(header.bytes)=$3 THEN substring(header.bytes FROM 1 FOR $3) END
 FROM doc_legacy_byte_stage_headers_v1 header WHERE header.doc_hash=$1 AND header.ordinal=$2`
	if !header {
		statement = `SELECT substring(part.hash FROM 1 FOR 72),
 CASE WHEN part.byte_count=$3 AND octet_length(object.bytes)=$3 THEN substring(object.bytes FROM 1 FOR $3) END
 FROM doc_legacy_byte_stage_parts_v1 part
 JOIN doc_legacy_byte_stage_objects_v1 object ON object.doc_hash=part.doc_hash AND object.hash=part.hash
 WHERE part.doc_hash=$1 AND part.ordinal=$2`
	}
	var hash domain.ContentHash
	var body []byte
	if err := transaction.QueryRow(ctx, statement, job.DocHash, ordinal, length).Scan(&hash, &body); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPreparedDocRangeObject
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if domain.ValidateContentHash(hash) != nil || len(body) != length {
		return nil, domain.ErrPreparedDocRangeObject
	}
	actual, err := domain.HashContentContext(ctx, body)
	if err != nil {
		return nil, err
	}
	if actual != hash {
		return nil, domain.ErrPreparedDocRangeObject
	}
	return body, nil
}

func legacyByteRecoveryReadError(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, failure error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if failure != nil && !errors.Is(failure, domain.ErrPreparedDocRangeObject) {
		return failure
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	return failure
}
