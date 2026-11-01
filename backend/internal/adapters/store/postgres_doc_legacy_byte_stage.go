//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyByteStage struct {
	sourceID      string
	incarnation   string
	sourceVersion int64
	plan          domain.ContentHash
	next, blocks  int
	complete      bool
}

type legacyByteBackfillSession struct {
	repo, hash domain.ContentHash
	stage      legacyByteStage
	plan       domain.LegacyByteBackfill
	encoded    []byte
	proof      domain.VerifiedSessionDoc
}

func (storage *PostgresStore) stageLegacyByteBackfill(ctx context.Context, job domain.DocRangeIndexJob, maxBlocks int) (legacyByteStage, error) {
	if maxBlocks < 1 || maxBlocks > domain.DocLegacyByteBackfillBatchBlocks {
		return legacyByteStage{}, domain.ErrValidation
	}
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		return legacyByteStage{}, err
	}
	return storage.stageLegacyByteSession(ctx, job, session, maxBlocks)
}

func (storage *PostgresStore) prepareLegacyByteBackfill(ctx context.Context, job domain.DocRangeIndexJob) (legacyByteBackfillSession, error) {
	if err := job.Validate(); err != nil {
		return legacyByteBackfillSession{}, err
	}
	if job.State != "running" || job.BaseHash != "" {
		return legacyByteBackfillSession{}, domain.ErrValidation
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	defer rollbackPG(transaction)
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteBackfillSession{}, err
	}
	var stage legacyByteStage
	var raw []byte
	err = transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text,
 substring(source.bytes FROM 1 FOR $3)
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,source`, job.RepoID, job.DocHash, 2*maxLegacyReadBytes+1).
		Scan(&stage.sourceVersion, &stage.sourceID, &raw)
	if err != nil {
		return legacyByteBackfillSession{}, mapNoRows(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyByteBackfillSession{}, err
	}
	session, preparationErr := storage.prepareLegacyByteSnapshot(ctx, job, stage, raw)
	if err := ctx.Err(); err != nil {
		return legacyByteBackfillSession{}, err
	}
	if err := storage.confirmLegacyBytePreparation(ctx, job, stage); err != nil {
		return legacyByteBackfillSession{}, err
	}
	if preparationErr != nil {
		return legacyByteBackfillSession{}, preparationErr
	}
	return session, nil
}

func (storage *PostgresStore) confirmLegacyBytePreparation(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage) error {
	confirmation, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(confirmation)
	var current legacyByteStage
	var locked bool
	err = confirmation.QueryRow(ctx, `WITH retained_source AS MATERIALIZED (
 SELECT source.doc_range_source_version AS version,incarnation.incarnation::text AS id
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,source
), retained_job AS MATERIALIZED (
 SELECT 1 FROM doc_range_jobs_v1 job CROSS JOIN retained_source source
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash=$5 AND source.version=$3 AND source.id=$4
 FOR UPDATE OF job
)
 SELECT source.version,source.id,EXISTS(SELECT 1 FROM retained_job) FROM retained_source source`,
		job.RepoID, job.DocHash, stage.sourceVersion, stage.sourceID, job.BaseHash).
		Scan(&current.sourceVersion, &current.sourceID, &locked)
	if err != nil {
		return mapNoRows(err)
	}
	if current.sourceVersion != stage.sourceVersion || current.sourceID != stage.sourceID || !locked {
		return domain.ErrConflict
	}
	if err := liveLegacyByteJob(ctx, confirmation, job); err != nil {
		return err
	}
	return confirmation.Commit(ctx)
}

func (storage *PostgresStore) prepareLegacyByteSnapshot(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage, raw []byte) (legacyByteBackfillSession, error) {
	body, err := boundedLegacyObject(ctx, raw, maxLegacyReadBytes)
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	return storage.prepareLegacyDecodedByteSnapshot(ctx, job, stage, body)
}

func (storage *PostgresStore) prepareLegacyDecodedByteSnapshot(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage, body []byte) (legacyByteBackfillSession, error) {
	if _, chunked := domain.ParseDocChunkManifest(body); chunked {
		return legacyByteBackfillSession{}, domain.ErrDocByteRangeBackfillRequired
	}
	proof, err := storage.verifyLegacyMonolithicRangeSource(ctx, job.DocHash, body)
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	plan, err := domain.NewLegacyByteBackfill(ctx, proof)
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	spec, err := plan.Spec()
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	encoded, err := spec.Encode()
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	stage.plan, err = plan.Key()
	if err != nil {
		return legacyByteBackfillSession{}, err
	}
	stage.blocks = spec.Blocks
	return legacyByteBackfillSession{repo: job.RepoID, hash: job.DocHash, stage: stage, plan: plan, encoded: encoded, proof: proof}, nil
}

func (storage *PostgresStore) verifyLegacyMonolithicRangeSource(ctx context.Context, hash domain.ContentHash, raw []byte) (domain.VerifiedSessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	actual, err := domain.HashContentContext(ctx, raw)
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if actual != hash {
		return verifyLegacyDecodedMonolithicDoc(ctx, hash, raw)
	}
	return storage.docProofs.rootVerifier.Verify(ctx, hash, raw)
}

func (storage *PostgresStore) stageLegacyByteSession(ctx context.Context, job domain.DocRangeIndexJob, session legacyByteBackfillSession, maxBlocks int) (legacyByteStage, error) {
	if err := job.Validate(); err != nil {
		return legacyByteStage{}, err
	}
	if job.State != "running" || job.BaseHash != "" || maxBlocks < 1 || maxBlocks > domain.DocLegacyByteBackfillBatchBlocks ||
		session.repo != job.RepoID || session.hash != job.DocHash || !session.plan.Valid() {
		return legacyByteStage{}, domain.ErrValidation
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return legacyByteStage{}, err
	}
	defer rollbackPG(transaction)
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteStage{}, err
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
		return legacyByteStage{}, err
	}
	var version int64
	var sourceID string
	err = transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned FOR UPDATE OF source`, job.RepoID, job.DocHash).Scan(&version, &sourceID)
	if err != nil {
		return legacyByteStage{}, mapNoRows(err)
	}
	if version != session.stage.sourceVersion || sourceID != session.stage.sourceID {
		return legacyByteStage{}, domain.ErrConflict
	}
	stage := session.stage
	prior, err := readLegacyByteStage(ctx, transaction, job.DocHash, session.encoded, stage)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return legacyByteStage{}, err
	}
	if err == nil {
		stage = prior
	} else {
		if err := transaction.QueryRow(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete) VALUES($1,$2::uuid,$3,$4,$5,$6,0,$7) RETURNING stage_id::text`,
			job.DocHash, stage.sourceID, stage.sourceVersion, stage.plan, session.encoded, stage.blocks, stage.blocks == 0).Scan(&stage.incarnation); err != nil {
			return legacyByteStage{}, err
		}
	}
	progress, err := session.plan.CopyBatch(ctx, stage.next, maxBlocks, func(ctx context.Context, block domain.VerifiedLegacyByteBlock) error {
		if !block.Valid() || block.Reference().Hash != job.DocHash || block.PlanKey() != stage.plan || block.Ordinal() < stage.next {
			return domain.ErrIntegrity
		}
		descriptor := block.Descriptor()
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_objects_v1(doc_hash,hash,bytes)
 VALUES($1,$2,$3) ON CONFLICT(doc_hash,hash) DO NOTHING`, job.DocHash, descriptor.Hash, block.Bytes()); err != nil {
			return err
		}
		_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_parts_v1(doc_hash,ordinal,hash,byte_count)
 VALUES($1,$2,$3,$4)`, job.DocHash, block.Ordinal(), descriptor.Hash, descriptor.Bytes)
		return err
	})
	if err != nil {
		return legacyByteStage{}, err
	}
	if progress.Next != stage.next {
		if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET next_block=$2,complete=$3 WHERE doc_hash=$1`, job.DocHash, progress.Next, progress.Complete); err != nil {
			return legacyByteStage{}, err
		}
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return legacyByteStage{}, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyByteStage{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyByteStage{}, err
	}
	stage.next, stage.complete = progress.Next, progress.Complete
	return stage, nil
}

func liveLegacyByteJob(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob) error {
	var live bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2
 AND base_hash='' AND state='running' AND version=$3 AND minimum_range_version=4 AND lease_until>clock_timestamp()
 AND EXISTS(SELECT 1 FROM repository_catalog_state catalog WHERE catalog.repo_id=$1 AND catalog.epoch::text=$4))`,
		job.RepoID, job.DocHash, job.Version, job.RepositoryEpoch).Scan(&live); err != nil {
		return err
	}
	if !live {
		return domain.ErrConflict
	}
	return ctx.Err()
}

func readLegacyByteStage(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash, expected []byte, current legacyByteStage) (legacyByteStage, error) {
	var prior legacyByteStage
	var spec []byte
	err := transaction.QueryRow(ctx, `SELECT source_id::text,source_version,COALESCE(substring(plan_hash FROM 1 FOR 72),''),
 substring(spec FROM 1 FOR $2),next_block,COALESCE(blocks,0),complete,stage_id::text FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR UPDATE`,
		hash, domain.MaxDocLegacyByteBackfillSpecBytes+1).
		Scan(&prior.sourceID, &prior.sourceVersion, &prior.plan, &spec, &prior.next, &prior.blocks, &prior.complete, &prior.incarnation)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && current.incarnation != "" {
			return legacyByteStage{}, domain.ErrConflict
		}
		return legacyByteStage{}, mapNoRows(err)
	}
	if current.incarnation != "" && prior.incarnation != current.incarnation {
		return legacyByteStage{}, domain.ErrConflict
	}
	if prior.sourceID != current.sourceID || prior.sourceVersion != current.sourceVersion {
		if current.incarnation != "" {
			return legacyByteStage{}, domain.ErrConflict
		}
		if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, hash); err != nil {
			return legacyByteStage{}, err
		}
		return legacyByteStage{}, domain.ErrNotFound
	}
	if prior.plan == "" {
		if prior.next != 0 || prior.blocks != 0 || prior.complete || spec != nil || domain.ValidateContentHash(current.plan) != nil {
			return legacyByteStage{}, domain.ErrPreparedDocRangeObject
		}
		if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
			return legacyByteStage{}, err
		}
		if err := transaction.QueryRow(ctx, `UPDATE doc_legacy_byte_stages_v1 SET plan_hash=$2,spec=$3,blocks=$4,complete=$5
 WHERE doc_hash=$1 AND stage_id=$6::uuid AND plan_hash IS NULL RETURNING stage_id::text`,
			hash, current.plan, expected, current.blocks, current.blocks == 0, prior.incarnation).Scan(&prior.incarnation); err != nil {
			return legacyByteStage{}, mapNoRows(err)
		}
		prior.plan, prior.blocks, prior.complete = current.plan, current.blocks, current.blocks == 0
		return prior, ctx.Err()
	}
	if prior.plan != current.plan || !bytes.Equal(spec, expected) || prior.blocks != current.blocks || prior.next < 0 ||
		prior.next > prior.blocks || prior.complete != (prior.next == prior.blocks) {
		return legacyByteStage{}, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteStageCorrupt)
	}
	return prior, ctx.Err()
}
