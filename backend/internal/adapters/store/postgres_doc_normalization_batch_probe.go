//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (work *legacyNormalizationCheckpointWork) deferBatch(ctx context.Context) (context.Context, *legacyNormalizationCheckpointBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if work.job.Validate() != nil || work.job.State != "running" || work.job.BaseHash != "" || work.state.stage.sourceVersion < 0 {
		return nil, nil, domain.ErrValidation
	}
	if _, nested := ctx.Value(repositoryTxKey{}).(*repositoryTx); nested || outbound.DocReadOnly(ctx) {
		return nil, nil, domain.ErrConflict
	}
	var epoch pgtype.UUID
	if epoch.Scan(work.job.RepositoryEpoch) != nil || !epoch.Valid || epoch.Bytes == [16]byte{} {
		return nil, nil, domain.ErrConflict
	}
	batch := &legacyNormalizationCheckpointBatch{owner: work, state: work.state, deferred: true}
	return context.WithValue(ctx, legacyNormalizationCheckpointBatchKey{}, batch), batch, nil
}

const legacyNormalizationDeferredBatchCurrent = `SELECT EXISTS(
 SELECT 1 FROM repos repo
 JOIN repository_catalog_state catalog ON catalog.repo_id=repo.id
 JOIN snapshots snapshot ON snapshot.repo_id=repo.id AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 JOIN repo_blobs owned ON owned.repo_id=repo.id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs original ON original.hash=owned.hash
 JOIN blob_source_incarnations source ON source.hash=original.hash
 JOIN doc_legacy_byte_stages_v1 stage ON stage.doc_hash=original.hash
 JOIN doc_range_jobs_v1 job ON job.repo_id=repo.id AND job.doc_hash=original.hash AND job.base_hash=''
 WHERE repo.id=$1 AND catalog.epoch::text=$3 AND source.incarnation::text=$4 AND original.doc_range_source_version=$5
 AND stage.source_id=source.incarnation AND stage.source_version=original.doc_range_source_version
 AND stage.stage_id::text=$6 AND stage.checkpoint_sequence=$7 AND COALESCE(stage.checkpoint_hash,'')=$8 AND stage.checkpoint_bytes=$9
 AND COALESCE(stage.plan_hash,'')=$10 AND COALESCE(stage.blocks,0)=$11 AND stage.next_block=$12 AND stage.complete=$13
 AND job.version=$14 AND job.state='running' AND job.minimum_range_version=4 AND job.lease_until>clock_timestamp())`

func (work *legacyNormalizationCheckpointWork) confirmDeferredBatch(ctx context.Context, expected legacyByteCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected != work.state || expected.stage.plan != "" {
		return domain.ErrConflict
	}
	job, stage := work.job, expected.stage
	var current bool
	err := work.store.pool.QueryRow(ctx, legacyNormalizationDeferredBatchCurrent,
		job.RepoID, job.DocHash, job.RepositoryEpoch, stage.sourceID, stage.sourceVersion,
		stage.incarnation, expected.sequence, expected.hash, expected.bytes, stage.plan, stage.blocks,
		stage.next, stage.complete, job.Version).Scan(&current)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !current {
		transaction, _, err := work.store.openLegacyByteCheckpoint(ctx, job, stage, &expected, false)
		if transaction != nil {
			rollbackPG(transaction)
		}
		if err != nil {
			return err
		}
		return domain.ErrConflict
	}
	return nil
}
