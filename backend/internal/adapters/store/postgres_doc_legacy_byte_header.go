//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const legacyByteHeaderPartBytes = 64 << 10

func (storage *PostgresStore) stageLegacyByteHeader(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage, seed domain.LegacyRangeMetadataSeed) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := job.Validate(); err != nil {
		return false, err
	}
	if job.State != "running" || job.BaseHash != "" || stage.sourceID == "" || stage.sourceVersion < 0 ||
		domain.ValidateContentHash(stage.plan) != nil || stage.blocks < 0 || stage.next < 0 || stage.next > stage.blocks ||
		stage.complete != (stage.next == stage.blocks) {
		return false, domain.ErrValidation
	}
	headerBytes := seed.EnvelopeBytes()
	if seed.Reference() != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) ||
		seed.PlanKey() != stage.plan || headerBytes < 2 || headerBytes > domain.MaxConversationDocumentBytes {
		return false, domain.ErrIntegrity
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return false, err
	}
	defer rollbackPG(transaction)
	var sourceVersion int64
	var sourceID string
	err = transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned FOR UPDATE OF source`, job.RepoID, job.DocHash).Scan(&sourceVersion, &sourceID)
	if err != nil {
		return false, mapNoRows(err)
	}
	if sourceVersion != stage.sourceVersion || sourceID != stage.sourceID {
		return false, domain.ErrConflict
	}
	var current legacyByteStage
	var encoded []byte
	var headerVersion, headerNext int
	var declaredBytes *int
	var headerComplete bool
	err = transaction.QueryRow(ctx, `SELECT source_id::text,source_version,COALESCE(substring(plan_hash FROM 1 FOR 72),''),
 substring(spec FROM 1 FOR $2),next_block,COALESCE(blocks,0),complete,stage_id::text,header_version,header_bytes,header_next,header_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR UPDATE`, job.DocHash, domain.MaxDocLegacyByteBackfillSpecBytes+1).
		Scan(&current.sourceID, &current.sourceVersion, &current.plan, &encoded, &current.next, &current.blocks, &current.complete,
			&current.incarnation, &headerVersion, &declaredBytes, &headerNext, &headerComplete)
	if err != nil {
		return false, mapNoRows(err)
	}
	if current.sourceID != sourceID || current.sourceVersion != sourceVersion || stage.incarnation != "" && stage.incarnation != current.incarnation {
		return false, domain.ErrConflict
	}
	if current.plan == "" {
		if err := lockDocRangeJob(ctx, transaction, job); err != nil {
			return false, err
		}
		if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
			return false, err
		}
		return false, domain.ErrDocRangeIndexPending
	}
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(current.plan, encoded)
	if err != nil || current.plan != stage.plan || current.blocks != stage.blocks || spec.Reference != seed.Reference() ||
		spec.Blocks != current.blocks || current.next < 0 || current.next > current.blocks || current.complete != (current.next == current.blocks) {
		return false, domain.ErrIntegrity
	}
	parts := (headerBytes + legacyByteHeaderPartBytes - 1) / legacyByteHeaderPartBytes
	switch headerVersion {
	case 0:
		if declaredBytes != nil || headerNext != 0 || headerComplete {
			return false, domain.ErrIntegrity
		}
	case 1:
		if declaredBytes == nil || *declaredBytes != headerBytes || headerNext < 0 || headerNext > parts || headerComplete != (headerNext == parts) {
			return false, domain.ErrIntegrity
		}
	default:
		return false, domain.ErrIntegrity
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return false, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return false, err
	}
	if headerComplete {
		if err := transaction.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
		return false, err
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_header_protocol='1'`); err != nil {
		return false, err
	}
	if headerVersion == 0 {
		tag, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=$2
 WHERE doc_hash=$1 AND header_version=0 AND header_bytes IS NULL AND header_next=0 AND NOT header_complete`, job.DocHash, headerBytes)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			return false, domain.ErrConflict
		}
	}
	offset := headerNext * legacyByteHeaderPartBytes
	length := min(legacyByteHeaderPartBytes, headerBytes-offset)
	body, err := seed.ReadEnvelope(ctx, offset, length)
	if err != nil {
		return false, err
	}
	if len(body) != length {
		return false, domain.ErrIntegrity
	}
	hash, err := domain.HashContentContext(ctx, body)
	if err != nil {
		return false, err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes)
 VALUES($1,$2,$3,$4)`, job.DocHash, headerNext, hash, body); err != nil {
		return false, err
	}
	headerComplete = headerNext+1 == parts
	tag, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=$4,header_complete=$5
 WHERE doc_hash=$1 AND header_version=1 AND header_bytes=$2 AND header_next=$3 AND NOT header_complete`,
		job.DocHash, headerBytes, headerNext, headerNext+1, headerComplete)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, domain.ErrConflict
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	return headerComplete, nil
}
