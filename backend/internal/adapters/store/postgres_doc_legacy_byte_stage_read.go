//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) readLegacyByteStageBlock(ctx context.Context, repo, hash domain.ContentHash, stage legacyByteStage, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if domain.ValidateContentHash(stage.plan) != nil || stage.sourceVersion < 0 || stage.sourceID == "" ||
		ordinal < 0 || ordinal >= stage.blocks || domain.ValidateContentHash(descriptor.Hash) != nil ||
		descriptor.Bytes <= 0 || descriptor.Bytes > domain.ConversationManifestChunkBytes {
		return nil, domain.ErrValidation
	}
	if !stage.complete || stage.next != stage.blocks {
		return nil, domain.ErrDocRangeIndexUnavailable
	}
	var body []byte
	err := storage.db(ctx).QueryRow(ctx, `SELECT CASE WHEN octet_length(object.bytes)=$7
 THEN substring(object.bytes FROM 1 FOR $7) END
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 JOIN doc_legacy_byte_stages_v1 stage ON stage.doc_hash=source.hash
 JOIN doc_legacy_byte_stage_parts_v1 part ON part.doc_hash=stage.doc_hash AND part.ordinal=$3
 JOIN doc_legacy_byte_stage_objects_v1 object ON object.doc_hash=part.doc_hash AND object.hash=part.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2 AND stage.complete
 AND incarnation.incarnation::text=$4 AND stage.source_id=incarnation.incarnation
 AND stage.source_version=source.doc_range_source_version AND stage.source_version=$5
 AND stage.plan_hash=$6 AND part.hash=$8 AND part.byte_count=$7`,
		repo, hash, ordinal, stage.sourceID, stage.sourceVersion, stage.plan, int(descriptor.Bytes), descriptor.Hash).Scan(&body)
	if err != nil {
		return nil, mapNoRows(err)
	}
	if int64(len(body)) != descriptor.Bytes || domain.HashContent(body) != descriptor.Hash {
		return nil, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}
