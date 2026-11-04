//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) ReadLegacyDerivedByteStorageUsage(ctx context.Context) (domain.LegacyDerivedByteStorageUsage, error) {
	var usage domain.LegacyDerivedByteStorageUsage
	if err := ctx.Err(); err != nil {
		return usage, err
	}
	err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if !storage.InReadOnlyTransaction(read) {
			return domain.ErrConflict
		}
		var metered int64
		err := storage.db(read).QueryRow(read, `SELECT count(*),count(*) FILTER(WHERE stage.complete),count(usage.doc_hash),
 coalesce(sum(usage.object_count),0)::bigint,coalesce(sum(usage.part_count),0)::bigint,
 coalesce(sum(usage.object_bytes),0)::bigint,coalesce(sum(octet_length(stage.spec)),0)::bigint,
 coalesce(sum(usage.header_bytes),0)::bigint,coalesce(sum(usage.header_parts),0)::bigint,
 coalesce(sum(usage.checkpoint_bytes),0)::bigint,coalesce(sum(usage.checkpoint_objects),0)::bigint,
 (SELECT sum(pg_table_size(relation))::bigint FROM unnest(ARRAY[
 'doc_legacy_byte_stages_v1'::regclass,'doc_legacy_byte_stage_objects_v1'::regclass,
 'doc_legacy_byte_stage_parts_v1'::regclass,'doc_legacy_byte_stage_headers_v1'::regclass,
 'doc_legacy_byte_usage_v1'::regclass,'doc_legacy_byte_stage_checkpoint_objects_v1'::regclass,
 'doc_legacy_checkpoint_staging_v1'::regclass,'doc_legacy_checkpoint_structures_v1'::regclass,'doc_legacy_checkpoint_edges_v1'::regclass]) relation),
 (SELECT sum(pg_indexes_size(relation))::bigint FROM unnest(ARRAY[
 'doc_legacy_byte_stages_v1'::regclass,'doc_legacy_byte_stage_objects_v1'::regclass,
 'doc_legacy_byte_stage_parts_v1'::regclass,'doc_legacy_byte_stage_headers_v1'::regclass,
 'doc_legacy_byte_usage_v1'::regclass,'doc_legacy_byte_stage_checkpoint_objects_v1'::regclass,
 'doc_legacy_checkpoint_staging_v1'::regclass,'doc_legacy_checkpoint_structures_v1'::regclass,'doc_legacy_checkpoint_edges_v1'::regclass]) relation)
 FROM doc_legacy_byte_stages_v1 stage LEFT JOIN doc_legacy_byte_usage_v1 usage ON usage.doc_hash=stage.doc_hash`).Scan(
			&usage.Sources, &usage.CompleteSources, &metered, &usage.Objects, &usage.Parts,
			&usage.PayloadBytes, &usage.SpecBytes, &usage.HeaderBytes, &usage.HeaderParts,
			&usage.CheckpointBytes, &usage.CheckpointObjects,
			&usage.TableBytes, &usage.IndexBytes)
		if err != nil {
			return err
		}
		if metered != usage.Sources {
			return domain.ErrIntegrity
		}
		return usage.Validate()
	})
	if err != nil {
		return domain.LegacyDerivedByteStorageUsage{}, err
	}
	return usage, nil
}

var _ outbound.LegacyDerivedByteStorageAccounting = (*PostgresStore)(nil)
