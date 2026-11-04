//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const legacyDerivedByteVerificationSQL = `SELECT CASE WHEN octet_length(object.bytes)=$4 THEN substring(object.bytes FROM 1 FOR $4) END
 FROM doc_legacy_byte_stage_parts_v1 part JOIN doc_legacy_byte_stage_objects_v1 object
 ON object.doc_hash=part.doc_hash AND object.hash=part.hash
 WHERE part.doc_hash=$1 AND part.ordinal=$2 AND part.hash=$3 AND part.byte_count=$4`

func (storage *PostgresStore) ConfigureLegacyDerivedByteRangePublication(enabled bool) {
	storage.legacyDerivedBytePublication = enabled
}

func (storage *PostgresStore) publishLegacyDerivedByteRange(ctx context.Context, job domain.DocRangeIndexJob, session legacyByteBackfillSession) (result error) {
	work, err := storage.prepareLegacyByteWork(ctx, job, session)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, work.Close()) }()
	return work.Publish(ctx, job)
}

func (storage *PostgresStore) publishLegacyDerivedByteWork(ctx context.Context, job domain.DocRangeIndexJob, work *preparedLegacyByteWork) error {
	if !storage.legacyByteRangePublication || !storage.legacyDerivedBytePublication {
		return domain.ErrConflict
	}
	if err := work.matches(job); err != nil {
		return err
	}
	session := work.session
	generation, err := work.metadata.Descriptor()
	if err != nil {
		return err
	}
	spec, err := session.plan.Spec()
	if err != nil {
		return err
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='5'`); err != nil {
		return err
	}
	var sourceVersion int64
	var sourceID string
	err = transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned FOR UPDATE OF source`, job.RepoID, job.DocHash).Scan(&sourceVersion, &sourceID)
	if err != nil {
		return mapNoRows(err)
	}
	if sourceVersion != session.stage.sourceVersion || sourceID != session.stage.sourceID {
		return domain.ErrConflict
	}
	stage, err := readLegacyByteStage(ctx, transaction, job.DocHash, session.encoded, session.stage)
	if err != nil {
		return err
	}
	if !stage.complete || stage.next != generation.ChunkCount() || generation.StreamBytes != spec.StreamBytes {
		return domain.ErrDocRangeIndexPending
	}
	unique := make(map[domain.ContentHash]int64)
	var payloadBytes int64
	for start := 0; start < stage.blocks; start += domain.DocLegacyByteBackfillBatchBlocks {
		descriptors, err := verifyLegacyDerivedByteBatch(ctx, transaction, job.DocHash, session.plan, start,
			min(start+domain.DocLegacyByteBackfillBatchBlocks, stage.blocks))
		if err != nil {
			return err
		}
		for _, descriptor := range descriptors {
			if prior, exists := unique[descriptor.Hash]; exists {
				if prior != descriptor.Bytes {
					return domain.ErrIntegrity
				}
			} else {
				unique[descriptor.Hash] = descriptor.Bytes
				payloadBytes += descriptor.Bytes
			}
		}
	}
	var storedBytes, storedObjects, storedParts int64
	if err := transaction.QueryRow(ctx, `SELECT object_bytes,object_count,part_count FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, job.DocHash).
		Scan(&storedBytes, &storedObjects, &storedParts); err != nil {
		return mapNoRows(err)
	}
	if storedBytes != payloadBytes || storedObjects != int64(len(unique)) || storedParts != int64(stage.blocks) {
		return domain.ErrIntegrity
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: job.RepoID})
	if err := storage.putLegacyRangeObjectReaderForPlan(bound, job.RepoID, generation, work.metadata.ObjectCount(), func(read context.Context, ordinal int) (domain.DocRangeObject, error) {
		return work.metadata.Object(read, ordinal, work.objects.Read)
	}, stage.plan); err != nil {
		return err
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state='completed',reason='',lease_until=NULL,byte_source_version=$4
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3 AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Version, sourceVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}

func verifyLegacyDerivedByteBatch(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash, plan domain.LegacyByteBackfill, start, end int) (_ []domain.ConversationManifestChunk, result error) {
	if start < 0 || end <= start || end-start > domain.DocLegacyByteBackfillBatchBlocks {
		return nil, domain.ErrValidation
	}
	batch := &pgx.Batch{}
	descriptors := make([]domain.ConversationManifestChunk, 0, end-start)
	for ordinal := start; ordinal < end; ordinal++ {
		block, err := plan.Block(ctx, ordinal)
		if err != nil {
			return nil, err
		}
		descriptor := block.Descriptor()
		descriptors = append(descriptors, descriptor)
		batch.Queue(legacyDerivedByteVerificationSQL, hash, ordinal, descriptor.Hash, int(descriptor.Bytes))
	}
	results := transaction.SendBatch(ctx, batch)
	defer func() { result = errors.Join(result, results.Close()) }()
	for _, descriptor := range descriptors {
		var raw []byte
		if err := results.QueryRow().Scan(&raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteStageCorrupt)
			}
			return nil, mapNoRows(err)
		}
		if int64(len(raw)) != descriptor.Bytes || domain.HashContent(raw) != descriptor.Hash {
			return nil, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteStageCorrupt)
		}
	}
	return descriptors, ctx.Err()
}

func (storage *PostgresStore) ReadLegacyDerivedByteChunk(ctx context.Context, repo, hash domain.ContentHash, binding domain.DocReadGenerationBinding, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if binding.Validate() != nil {
		return nil, domain.ErrValidation
	}
	var stage legacyByteStage
	err := storage.db(ctx).QueryRow(ctx, `SELECT stage.source_id::text,stage.source_version,stage.plan_hash,stage.next_block,stage.blocks,stage.complete
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 JOIN doc_range_generations_v2 generation ON generation.doc_hash=source.hash
 JOIN doc_legacy_byte_stages_v1 stage ON stage.doc_hash=source.hash AND stage.plan_hash=generation.legacy_byte_plan_hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2 AND generation.generation_id::text=$3 AND generation.generation_hash=$4
 AND generation.source_version=$5 AND generation.source_version=source.doc_range_source_version
 AND stage.source_id=incarnation.incarnation AND stage.source_version=generation.source_version AND stage.complete`,
		repo, hash, binding.SourceID, binding.RangeGeneration, binding.SourceVersion).
		Scan(&stage.sourceID, &stage.sourceVersion, &stage.plan, &stage.next, &stage.blocks, &stage.complete)
	if err != nil {
		return nil, mapNoRows(err)
	}
	return storage.readLegacyByteStageBlock(ctx, repo, hash, stage, ordinal, descriptor)
}

var _ outbound.LegacyDerivedByteRangePublicationStore = (*PostgresStore)(nil)
var _ outbound.LegacyDerivedByteRangeReader = (*PostgresStore)(nil)
