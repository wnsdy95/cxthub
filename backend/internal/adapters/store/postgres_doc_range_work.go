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

type preparedLegacyByteWork struct {
	store         *PostgresStore
	job           domain.DocRangeIndexJob
	session       legacyByteBackfillSession
	metadata      domain.VerifiedLegacyDerivedRangeMetadata
	preparation   *domain.LegacyRangeMetadataPreparation
	spooling      *domain.LegacyByteSpoolPreparation
	header        domain.LegacyRangeMetadataSeed
	data          *legacyDocSpool
	objects       *docRangeSpool
	recoveryReady bool
	closed        bool
	once          sync.Once
	err           error
}

func (storage *PostgresStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if job.State != "running" {
		return nil, domain.ErrValidation
	}
	if _, nested := ctx.Value(repositoryTxKey{}).(*repositoryTx); nested || outbound.DocReadOnly(ctx) {
		return nil, domain.ErrConflict
	}
	if job.BaseHash != "" || !storage.legacyByteRangePublication || !storage.legacyDerivedBytePublication {
		return nil, nil
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return nil, err
	}
	defer rollbackPG(transaction)
	var minimum int
	err = transaction.QueryRow(ctx, `SELECT minimum_range_version FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3 AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Version).Scan(&minimum)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	var identity domain.DocumentIdentity
	if err := transaction.QueryRow(ctx, docRangeJobIdentitySQL+` FOR SHARE OF snapshot`, job.RepoID, job.DocHash,
		domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy).Scan(&identity); err != nil {
		return nil, mapNoRows(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	if minimum < 4 || identity != domain.DocumentIdentityLegacy {
		return nil, nil
	}
	recovered, err := storage.newLegacyRecoveryWork(ctx, job)
	if err != nil {
		return nil, err
	}
	if recovered != nil {
		return recovered, nil
	}
	work, err := storage.newLegacyCaptureWork(ctx, job)
	if err != nil {
		return nil, err
	}
	return work, nil
}

func (storage *PostgresStore) prepareLegacyByteWork(ctx context.Context, job domain.DocRangeIndexJob, session legacyByteBackfillSession) (_ *preparedLegacyByteWork, result error) {
	work, err := storage.newLegacyByteWork(ctx, job, session)
	if err != nil {
		return nil, err
	}
	for {
		progress, err := work.Step(ctx, job)
		if err != nil {
			return nil, errors.Join(err, work.Close())
		}
		if progress == outbound.DocRangeWorkPublishable {
			break
		}
	}
	return work, nil
}

func (storage *PostgresStore) newLegacyByteWork(ctx context.Context, job domain.DocRangeIndexJob, session legacyByteBackfillSession) (_ *preparedLegacyByteWork, result error) {
	if session.repo != job.RepoID || session.hash != job.DocHash || !session.plan.Valid() || !session.proof.Valid() ||
		session.proof.DocumentRef() != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) ||
		domain.HashContent(session.encoded) != session.stage.plan {
		return nil, domain.ErrIntegrity
	}
	data, err := newLegacyDocSpool()
	if err != nil {
		return nil, err
	}
	objects, err := newDocRangeSpool()
	if err != nil {
		return nil, errors.Join(err, data.Close())
	}
	prepared := false
	defer func() {
		if !prepared {
			result = errors.Join(result, data.Close(), objects.Close())
		}
	}()
	header, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil {
		return nil, err
	}
	spooling, err := domain.NewLegacyByteSpoolPreparation(ctx, session.plan, data)
	if err != nil {
		return nil, err
	}
	if header.Reference() != session.proof.DocumentRef() || header.PlanKey() != session.stage.plan {
		return nil, domain.ErrIntegrity
	}
	if err := objects.Flush(ctx); err != nil {
		return nil, err
	}
	work := &preparedLegacyByteWork{store: storage, job: job, session: session, spooling: spooling, header: header, data: data, objects: objects}
	work, err = initializeLegacyByteWork(ctx, work)
	if err != nil {
		return nil, err
	}
	prepared = true
	return work, nil
}

func initializeLegacyByteWork(ctx context.Context, work *preparedLegacyByteWork) (*preparedLegacyByteWork, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session := &work.session
	session.proof, session.encoded = domain.VerifiedSessionDoc{}, bytes.Clone(session.encoded)
	session.repo, session.hash = domain.ContentHash(strings.Clone(string(session.repo))), domain.ContentHash(strings.Clone(string(session.hash)))
	session.stage.plan = domain.ContentHash(strings.Clone(string(session.stage.plan)))
	session.stage.sourceID = strings.Clone(session.stage.sourceID)
	session.stage.incarnation = strings.Clone(session.stage.incarnation)
	work.job.RepoID, work.job.DocHash = session.repo, session.hash
	work.job.State, work.job.Reason, work.job.BaseHash = strings.Clone(work.job.State), strings.Clone(work.job.Reason), domain.ContentHash(strings.Clone(string(work.job.BaseHash)))
	work.job.RepositoryEpoch = strings.Clone(work.job.RepositoryEpoch)
	if _, err := work.RetainedResources(); err != nil {
		return nil, err
	}
	return work, ctx.Err()
}

func (storage *PostgresStore) newLegacySourceByteWork(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage, proof domain.VerifiedLegacyByteSource) (_ *preparedLegacyByteWork, failure error) {
	plan, err := proof.Plan()
	if err != nil {
		return nil, err
	}
	header, err := proof.Seed()
	if err != nil {
		return nil, err
	}
	source, supported := proof.Source().(*legacyProofSpool)
	if !supported || source == nil || source.legacyDocSpool == nil {
		return nil, domain.ErrIntegrity
	}
	data := source.legacyDocSpool
	spec, err := plan.Spec()
	if err != nil || spec.Reference != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) {
		return nil, errors.Join(domain.ErrIntegrity, err)
	}
	info, err := data.file.Stat()
	if err != nil || info.Size() != int64(data.size) || info.Size() != spec.StreamBytes {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	encoded, err := spec.Encode()
	if err != nil {
		return nil, err
	}
	stage.plan, err = plan.Key()
	if err != nil {
		return nil, err
	}
	stage.blocks = spec.Blocks
	preparation, err := header.NewPreparation(ctx, plan)
	if err != nil {
		return nil, err
	}
	objects, err := newDocRangeSpool()
	if err != nil {
		return nil, err
	}
	defer func() {
		if failure != nil {
			failure = errors.Join(failure, objects.Close())
		}
	}()
	if err := objects.Flush(ctx); err != nil {
		return nil, err
	}
	work, err := initializeLegacyByteWork(ctx, &preparedLegacyByteWork{store: storage, job: job,
		session:     legacyByteBackfillSession{repo: job.RepoID, hash: job.DocHash, stage: stage, plan: plan, encoded: encoded},
		preparation: preparation, header: header, data: data, objects: objects})
	if err != nil {
		return nil, err
	}
	return work, nil
}

func (work *preparedLegacyByteWork) matches(job domain.DocRangeIndexJob) error {
	if work == nil || work.closed || work.data == nil || work.objects == nil || work.objects.writer == nil {
		return os.ErrClosed
	}
	if job.Validate() != nil || job.State != "running" || job.RepoID != work.job.RepoID || job.DocHash != work.job.DocHash ||
		job.BaseHash != "" || job.Version != work.job.Version || job.RepositoryEpoch != work.job.RepositoryEpoch {
		return domain.ErrConflict
	}
	if work.store == nil || !work.session.plan.Valid() || work.session.proof.Valid() {
		return domain.ErrIntegrity
	}
	reference, key := work.metadata.Reference(), work.metadata.PlanKey()
	if work.spooling != nil {
		if work.preparation != nil || work.metadata.Valid() || work.header.Reference() != work.spooling.Reference() || work.header.PlanKey() != work.spooling.PlanKey() {
			return domain.ErrIntegrity
		}
		reference, key = work.spooling.Reference(), work.spooling.PlanKey()
	}
	if work.preparation != nil {
		if work.metadata.Valid() {
			return domain.ErrIntegrity
		}
		reference, key = work.preparation.Reference(), work.preparation.PlanKey()
	}
	if reference != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) || key != work.session.stage.plan {
		return domain.ErrIntegrity
	}
	if !work.recoveryReady && (work.header.Reference() != reference || work.header.PlanKey() != key || work.header.EnvelopeBytes() == 0) {
		return domain.ErrIntegrity
	}
	return nil
}

func (work *preparedLegacyByteWork) advanceSpooling(ctx context.Context) error {
	complete, err := work.spooling.Step(ctx)
	if err != nil || !complete {
		return err
	}
	plan, err := work.spooling.Plan()
	if err != nil {
		return err
	}
	preparation, err := work.header.NewPreparation(ctx, plan)
	if err != nil {
		return err
	}
	work.session.plan, work.preparation = plan, preparation
	work.spooling = nil
	return nil
}

func (work *preparedLegacyByteWork) advanceMetadata(ctx context.Context) error {
	if work.preparation == nil {
		return ctx.Err()
	}
	complete, err := work.preparation.Step(ctx, func(write context.Context, object domain.DocRangeObject) error {
		return work.objects.Append(write, object.Bytes)
	})
	if err != nil {
		return err
	}
	if err := work.objects.Flush(ctx); err != nil {
		return err
	}
	if complete {
		work.metadata, err = work.preparation.Metadata()
		if err != nil {
			return err
		}
		work.preparation = nil
	}
	return ctx.Err()
}

func (work *preparedLegacyByteWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	if err := work.matches(job); err != nil {
		return 0, err
	}
	if work.spooling != nil {
		if err := work.store.confirmLegacyBytePreparation(ctx, job, work.session.stage); err != nil {
			return 0, err
		}
		return outbound.DocRangeWorkPending, work.advanceSpooling(ctx)
	}
	stage, err := work.store.stageLegacyByteSession(ctx, job, work.session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil {
		return 0, err
	}
	work.session.stage = stage
	if !work.recoveryReady {
		work.recoveryReady, err = work.store.stageLegacyByteHeader(ctx, job, stage, work.header)
		if err != nil {
			return 0, err
		}
		if work.recoveryReady {
			work.header = domain.LegacyRangeMetadataSeed{}
		} else {
			return outbound.DocRangeWorkPending, nil
		}
	}
	if err := work.advanceMetadata(ctx); err != nil {
		return 0, err
	}
	if stage.complete && work.recoveryReady && work.metadata.Valid() {
		return outbound.DocRangeWorkPublishable, nil
	}
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyByteWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := work.matches(job); err != nil {
		return err
	}
	if work.spooling != nil || work.preparation != nil || !work.recoveryReady {
		return domain.ErrDocRangeIndexPending
	}
	return work.store.publishLegacyDerivedByteWork(ctx, job, work)
}

func (work *preparedLegacyByteWork) RetainedResources() (outbound.RootPublicationResources, error) {
	if work == nil {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	if err := work.matches(work.job); err != nil {
		return outbound.RootPublicationResources{}, err
	}
	data, err := work.data.file.Stat()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	objects, err := work.objects.file.Stat()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	spec, err := work.session.plan.Spec()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	objectCount := work.metadata.ObjectCount()
	if work.preparation != nil {
		objectCount = work.preparation.ObjectCount()
	}
	expectedBytes := spec.StreamBytes
	if work.spooling != nil {
		expectedBytes = work.spooling.SourceBytes()
	}
	if data.Size() != int64(work.data.size) || data.Size() != expectedBytes || data.Size() < 0 || objects.Size() < 0 {
		return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
	}
	if data.Size() > math.MaxInt64-objects.Size() || len(work.objects.offsets) == 0 || len(work.objects.offsets)-1 != objectCount {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	if objects.Size() != work.objects.offsets[len(work.objects.offsets)-1] {
		return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
	}
	planBytes, err := work.session.plan.RetainedBytes()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	var metadataBytes int64
	if work.spooling != nil {
		spoolingBytes, spoolErr := work.spooling.RetainedBytes()
		if spoolErr != nil {
			return outbound.RootPublicationResources{}, spoolErr
		}
		headerBytes, headerErr := work.header.RetainedBytes()
		if headerErr != nil {
			return outbound.RootPublicationResources{}, headerErr
		}
		if spoolingBytes < 0 || headerBytes < 0 || spoolingBytes > math.MaxInt64-headerBytes {
			return outbound.RootPublicationResources{}, domain.ErrIntegrity
		}
		metadataBytes = spoolingBytes + headerBytes
	} else if work.preparation != nil {
		metadataBytes, err = work.preparation.RetainedBytes()
	} else {
		metadataBytes, err = work.metadata.RetainedBytes()
	}
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	if planBytes < 0 || metadataBytes < 0 || planBytes > math.MaxInt64-metadataBytes {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	estimate := rootPublicationHeapEstimate{bytes: planBytes + metadataBytes}
	estimate.allocation(1, 8192+int64(unsafe.Sizeof(*work))+int64(unsafe.Sizeof(*work.data))+int64(unsafe.Sizeof(*work.objects)))
	estimate.allocation(int64(cap(work.session.encoded)), 1)
	estimate.allocation(int64(cap(work.objects.offsets)), int64(unsafe.Sizeof(int64(0))))
	estimate.allocation(int64(work.objects.writer.Size()), 1)
	for _, value := range []string{string(work.job.RepoID), string(work.job.DocHash), string(work.job.BaseHash), work.job.State, work.job.Reason, work.job.RepositoryEpoch,
		string(work.session.stage.plan), work.session.stage.sourceID, work.session.stage.incarnation, work.data.file.Name(), work.objects.file.Name()} {
		estimate.allocation(int64(len(value)), 1)
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	return outbound.RootPublicationResources{HeapBytes: estimate.bytes, SpoolBytes: data.Size() + objects.Size()}, nil
}

func (work *preparedLegacyByteWork) Close() error {
	if work == nil {
		return nil
	}
	work.once.Do(func() {
		work.closed = true
		work.err = errors.Join(work.data.Close(), work.objects.Close())
		work.session = legacyByteBackfillSession{}
		work.metadata = domain.VerifiedLegacyDerivedRangeMetadata{}
		work.preparation = nil
		work.spooling, work.header = nil, domain.LegacyRangeMetadataSeed{}
	})
	return work.err
}

var _ outbound.DocRangeWorkStore = (*PostgresStore)(nil)
var _ outbound.PreparedDocRangeWork = (*preparedLegacyByteWork)(nil)
