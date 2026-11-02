//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var errLegacyByteStageCorrupt = errors.New("durable legacy byte stage is corrupt")
var errLegacyByteRecoveryLocal = errors.New("local legacy recovery spool is corrupt")

type preparedLegacyRecoveryWork struct {
	store     *PostgresStore
	job       domain.DocRangeIndexJob
	state     legacyByteRecoveryState
	source    domain.LegacyCanonicalDocumentSource
	input     *legacyDocSpool
	parts     []domain.ConversationManifestChunk
	canonical *domain.LegacyByteSourcePreparation
	data      *legacyDocSpool
	work      *preparedLegacyByteWork
	closed    bool
	once      sync.Once
	err       error
}

func (storage *PostgresStore) newLegacyRecoveryWork(ctx context.Context, job domain.DocRangeIndexJob) (_ *preparedLegacyRecoveryWork, failure error) {
	state, ready, err := storage.readLegacyByteRecovery(ctx, job)
	if err != nil {
		if errors.Is(err, domain.ErrPreparedDocRangeObject) {
			if discard := storage.discardLegacyByteRecovery(ctx, job, state.stage); discard != nil {
				return nil, discard
			}
		}
		return nil, err
	}
	if !ready {
		return nil, nil
	}
	checkpoint, pinned, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil {
		return nil, err
	}
	if pinned {
		if checkpoint.stage.incarnation != state.stage.incarnation || checkpoint.stage.plan != state.stage.plan {
			return nil, domain.ErrConflict
		}
		if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, checkpoint); err != nil {
			return nil, err
		}
	}
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(state.stage.plan, state.encoded)
	if err != nil || spec.Reference != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) || spec.Blocks != state.stage.blocks {
		if discard := storage.discardLegacyByteRecovery(ctx, job, state.stage); discard != nil {
			return nil, discard
		}
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	input, err := newLegacyDocSpool()
	if err != nil {
		return nil, err
	}
	job.RepoID, job.DocHash = domain.ContentHash(strings.Clone(string(job.RepoID))), domain.ContentHash(strings.Clone(string(job.DocHash)))
	job.State, job.Reason, job.BaseHash = strings.Clone(job.State), strings.Clone(job.Reason), domain.ContentHash(strings.Clone(string(job.BaseHash)))
	job.RepositoryEpoch = strings.Clone(job.RepositoryEpoch)
	state.encoded = bytes.Clone(state.encoded)
	state.stage.plan = domain.ContentHash(strings.Clone(string(state.stage.plan)))
	state.stage.sourceID = strings.Clone(state.stage.sourceID)
	state.stage.incarnation = strings.Clone(state.stage.incarnation)
	work := &preparedLegacyRecoveryWork{store: storage, job: job, state: state, input: input}
	defer func() {
		if failure != nil {
			failure = errors.Join(failure, work.Close())
		}
	}()
	work.source = domain.LegacyCanonicalDocumentSource{EnvelopeBytes: state.headerBytes, StreamBytes: int(spec.StreamBytes),
		ReadEnvelope: func(read context.Context, offset, length int) ([]byte, error) {
			return work.read(read, true, offset, length)
		},
		ReadStream: func(read context.Context, offset, length int) ([]byte, error) {
			return work.read(read, false, offset, length)
		}}
	if _, err := work.source.Size(); err != nil {
		return nil, work.fail(ctx, err)
	}
	if _, err := work.RetainedResources(); err != nil {
		return nil, err
	}
	return work, ctx.Err()
}

func (storage *PostgresStore) discardLegacyByteRecovery(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteStage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	var current legacyByteStage
	if err := transaction.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM snapshots snapshot JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned FOR UPDATE OF source`, job.RepoID, job.DocHash).Scan(&current.sourceVersion, &current.sourceID); err != nil {
		return mapNoRows(err)
	}
	if current.sourceVersion != expected.sourceVersion || current.sourceID != expected.sourceID {
		return domain.ErrConflict
	}
	var stored legacyByteStage
	var checkpointPinned bool
	err = transaction.QueryRow(ctx, `SELECT source_version,source_id::text,COALESCE(plan_hash,''),stage_id::text,
 checkpoint_hash IS NOT NULL FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR UPDATE`, job.DocHash).
		Scan(&stored.sourceVersion, &stored.sourceID, &stored.plan, &stored.incarnation, &checkpointPinned)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	present := err == nil
	if present && (stored.plan == "" || checkpointPinned) {
		return domain.ErrConflict
	}
	if present && (stored.sourceVersion != expected.sourceVersion || stored.sourceID != expected.sourceID || stored.plan != expected.plan || stored.incarnation != expected.incarnation || expected.incarnation == "") {
		return domain.ErrConflict
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return err
	}
	if present {
		if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 AND source_id=$2::uuid AND source_version=$3 AND plan_hash=$4 AND stage_id=$5::uuid`,
			job.DocHash, expected.sourceID, expected.sourceVersion, expected.plan, expected.incarnation); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func (work *preparedLegacyRecoveryWork) matches(job domain.DocRangeIndexJob) error {
	if work == nil || work.closed || work.store == nil {
		return os.ErrClosed
	}
	if job.Validate() != nil || job.State != "running" || job.BaseHash != "" || job.RepoID != work.job.RepoID || job.DocHash != work.job.DocHash || job.Version != work.job.Version || job.RepositoryEpoch != work.job.RepositoryEpoch {
		return domain.ErrConflict
	}
	return nil
}

func (work *preparedLegacyRecoveryWork) fail(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.store.confirmLegacyBytePreparation(ctx, work.job, work.state.stage); err != nil {
		return err
	}
	if errors.Is(cause, errLegacyByteRecoveryLocal) {
		return errors.Join(domain.ErrPreparedDocRangeObject, cause)
	}
	if errors.Is(cause, domain.ErrPreparedDocRangeObject) || errors.Is(cause, domain.ErrIntegrity) || errors.Is(cause, domain.ErrUnsupportedCIRVersion) {
		if err := work.store.discardLegacyByteRecovery(ctx, work.job, work.state.stage); err != nil {
			return err
		}
		return errors.Join(domain.ErrPreparedDocRangeObject, cause)
	}
	return cause
}

func (work *preparedLegacyRecoveryWork) read(ctx context.Context, header bool, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := work.matches(work.job); err != nil {
		return nil, err
	}
	partBytes, extent := domain.ConversationManifestChunkBytes, work.source.StreamBytes
	if header {
		partBytes, extent = domain.LegacyCanonicalEncodingBytes, work.source.EnvelopeBytes
	}
	if offset < 0 || length < 0 || length > domain.ConversationManifestChunkBytes || offset > extent || length > extent-offset {
		return nil, domain.ErrIntegrity
	}
	body := make([]byte, length)
	for copied := 0; copied < length; {
		position := offset + copied
		part, err := work.store.readLegacyByteRecoveryPart(ctx, work.job, work.state, header, position/partBytes)
		if err != nil {
			return nil, err
		}
		if len(part) != min(partBytes, extent-position/partBytes*partBytes) || position%partBytes >= len(part) {
			return nil, domain.ErrPreparedDocRangeObject
		}
		copied += copy(body[copied:], part[position%partBytes:])
	}
	return body, ctx.Err()
}

func (work *preparedLegacyRecoveryWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (progress outbound.DocRangeWorkProgress, failure error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := work.matches(job); err != nil {
		return 0, err
	}
	if work.work != nil {
		progress, err := work.work.Step(ctx, job)
		if err != nil {
			if errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, errLegacyByteStageCorrupt) {
				err = errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
			}
			return 0, work.fail(ctx, err)
		}
		return progress, nil
	}
	if err := work.store.confirmLegacyBytePreparation(ctx, job, work.state.stage); err != nil {
		return 0, err
	}
	defer func() {
		if failure != nil {
			progress, failure = 0, work.fail(ctx, failure)
		}
	}()
	extent, err := work.source.Size()
	if err != nil {
		return 0, err
	}
	if work.input == nil || work.input.size < 0 || work.input.size > extent {
		return 0, domain.ErrPreparedDocRangeObject
	}
	if work.input.size < extent {
		body, err := work.source.Read(ctx, work.input.size, min(domain.ConversationManifestChunkBytes, extent-work.input.size))
		if err != nil {
			return 0, err
		}
		hash, err := domain.HashContentContext(ctx, body)
		if err != nil {
			return 0, err
		}
		if err := work.input.Append(ctx, body); err != nil {
			return 0, err
		}
		work.parts = append(work.parts, domain.ConversationManifestChunk{Hash: hash, Bytes: int64(len(body))})
		return outbound.DocRangeWorkPending, nil
	}
	if work.canonical == nil {
		data, err := newLegacyDocSpool()
		if err != nil {
			return 0, err
		}
		work.data = data
		work.canonical, err = domain.NewLegacyByteSourcePreparation(ctx, job.DocHash,
			domain.LegacyByteSource{Bytes: extent, Parts: work.parts, Read: work.readInput},
			&legacyProofSpool{legacyDocSpool: data}, domain.NewCanonicalDocVerifierWithoutCache())
		return outbound.DocRangeWorkPending, err
	}
	status, err := work.canonical.Step(ctx)
	if err != nil {
		return 0, err
	}
	if status == domain.LegacyByteSourcePending {
		return outbound.DocRangeWorkPending, nil
	}
	if status != domain.LegacyByteSourceVerified {
		return 0, domain.ErrPreparedDocRangeObject
	}
	proof, err := work.canonical.Result()
	if err != nil {
		return 0, err
	}
	plan, err := proof.Plan()
	if err != nil {
		return 0, err
	}
	spec, err := plan.Spec()
	if err != nil {
		return 0, err
	}
	encoded, err := spec.Encode()
	if err != nil || !bytes.Equal(encoded, work.state.encoded) {
		return 0, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	prepared, err := work.store.newLegacySourceByteWork(ctx, job, work.state.stage, proof)
	if err != nil {
		return 0, err
	}
	work.work, work.data, work.canonical, work.parts = prepared, nil, nil, nil
	if err := work.input.Close(); err != nil {
		return 0, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	work.input, work.source = nil, domain.LegacyCanonicalDocumentSource{}
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyRecoveryWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.matches(job); err != nil {
		return err
	}
	if work.work == nil {
		return domain.ErrDocRangeIndexPending
	}
	if err := work.work.Publish(ctx, job); err != nil {
		if errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, errLegacyByteStageCorrupt) {
			err = errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
		}
		return work.fail(ctx, err)
	}
	return nil
}

func (work *preparedLegacyRecoveryWork) readInput(ctx context.Context, offset, length int) ([]byte, error) {
	body, err := checkedLegacySpoolRead(ctx, work.input, offset, length)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		if work.input != nil && offset == work.input.size && length > 0 && errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrPreparedDocRangeObject) {
			return nil, err
		}
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
	}
	if length == 0 {
		return body, nil
	}
	ordinal := offset / domain.ConversationManifestChunkBytes
	if offset < 0 || offset%domain.ConversationManifestChunkBytes != 0 || ordinal >= len(work.parts) || int64(length) != work.parts[ordinal].Bytes {
		return nil, domain.ErrIntegrity
	}
	actual, err := domain.HashContentContext(ctx, body)
	if err != nil {
		return nil, err
	}
	if actual != work.parts[ordinal].Hash {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal)
	}
	return body, ctx.Err()
}

func (work *preparedLegacyRecoveryWork) RetainedResources() (outbound.RootPublicationResources, error) {
	if work == nil {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	if err := work.matches(work.job); err != nil {
		return outbound.RootPublicationResources{}, err
	}
	var resources outbound.RootPublicationResources
	var err error
	if work.work != nil {
		resources, err = work.work.RetainedResources()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
	}
	estimate := rootPublicationHeapEstimate{bytes: resources.HeapBytes}
	estimate.allocation(1, 8192+int64(unsafe.Sizeof(*work)))
	estimate.allocation(int64(cap(work.state.encoded)), 1)
	estimate.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(domain.ConversationManifestChunk{})))
	for _, part := range work.parts {
		estimate.allocation(int64(len(part.Hash)), 1)
	}
	for _, value := range []string{string(work.job.RepoID), string(work.job.DocHash), string(work.job.BaseHash), work.job.State, work.job.Reason, work.job.RepositoryEpoch, string(work.state.stage.plan), work.state.stage.sourceID, work.state.stage.incarnation} {
		estimate.allocation(int64(len(value)), 1)
	}
	for _, spool := range []*legacyDocSpool{work.input, work.data} {
		if spool != nil {
			info, err := spool.file.Stat()
			if err != nil || spool.size < 0 || info.Size() != int64(spool.size) || resources.SpoolBytes > math.MaxInt64-int64(spool.size) {
				return outbound.RootPublicationResources{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
			}
			resources.SpoolBytes += int64(spool.size)
			estimate.allocation(1, int64(unsafe.Sizeof(*spool)))
			estimate.allocation(int64(len(spool.file.Name())), 1)
		}
	}
	if work.canonical != nil {
		retained, err := work.canonical.RetainedBytes()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
		if retained < 0 || estimate.bytes > math.MaxInt64-retained {
			return outbound.RootPublicationResources{}, domain.ErrIntegrity
		}
		estimate.bytes += retained
		estimate.allocation(1, int64(unsafe.Sizeof(domain.CanonicalDocVerifier{})))
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	resources.HeapBytes = estimate.bytes
	return resources, nil
}

func (work *preparedLegacyRecoveryWork) Close() error {
	if work == nil {
		return nil
	}
	work.once.Do(func() {
		work.closed = true
		if work.work != nil {
			work.err = work.work.Close()
		}
		for _, spool := range []*legacyDocSpool{work.input, work.data} {
			if spool != nil {
				work.err = errors.Join(work.err, spool.Close())
			}
		}
		work.work, work.input, work.data, work.parts, work.canonical = nil, nil, nil, nil, nil
		work.source, work.state = domain.LegacyCanonicalDocumentSource{}, legacyByteRecoveryState{}
	})
	return work.err
}

var _ outbound.PreparedDocRangeWork = (*preparedLegacyRecoveryWork)(nil)
