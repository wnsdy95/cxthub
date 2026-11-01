//go:build postgres

package store

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyCaptureSource struct {
	stage legacyByteStage
	bytes int
}

type preparedLegacyCaptureWork struct {
	store             *PostgresStore
	job               domain.DocRangeIndexJob
	witness           legacyCaptureSource
	source            *legacyDocSpool
	parts             []legacyCapturePart
	partHeap          rootPublicationHeapEstimate
	decoding          *legacyCaptureDecodeWork
	normalization     *legacyNormalizationWork
	checkpoint        *legacyNormalizationCheckpointWork
	canonical         *domain.LegacyByteSourcePreparation
	canonicalProof    domain.VerifiedLegacyByteSource
	proofData         *legacyDocSpool
	original          bool
	work              *preparedLegacyByteWork
	compatibility     bool
	parserSteps       int
	encoderSteps      int
	encoderGeneration uint64
	closed            bool
	once              sync.Once
	err               error
}

func (storage *PostgresStore) readLegacyCaptureSource(ctx context.Context, job domain.DocRangeIndexJob, expected *legacyCaptureSource, offset, length int) (legacyCaptureSource, []byte, error) {
	if err := ctx.Err(); err != nil {
		return legacyCaptureSource{}, nil, err
	}
	if offset < 0 || length < 0 || length > domain.ConversationManifestChunkBytes ||
		expected == nil && (offset != 0 || length != 0) ||
		expected != nil && (offset > expected.bytes || length > expected.bytes-offset) {
		return legacyCaptureSource{}, nil, domain.ErrIntegrity
	}
	transaction, err := storage.beginBoundDocRangeJob(ctx, job)
	if err != nil {
		return legacyCaptureSource{}, nil, err
	}
	defer rollbackPG(transaction)
	var witness legacyCaptureSource
	var body []byte
	selection := `SELECT source.doc_range_source_version,incarnation.incarnation::text,octet_length(source.bytes)`
	arguments := []any{job.RepoID, job.DocHash}
	values := []any{&witness.stage.sourceVersion, &witness.stage.sourceID, &witness.bytes}
	if length != 0 {
		selection += `,substring(source.bytes FROM $3 FOR $4)`
		arguments = append(arguments, offset+1, length)
		values = append(values, &body)
	}
	err = transaction.QueryRow(ctx, selection+`
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 FOR SHARE OF snapshot,owned,source`, arguments...).Scan(values...)
	if err != nil {
		return legacyCaptureSource{}, nil, mapNoRows(err)
	}
	if expected != nil && (witness.stage.sourceVersion != expected.stage.sourceVersion ||
		witness.stage.sourceID != expected.stage.sourceID || witness.bytes != expected.bytes) {
		return legacyCaptureSource{}, nil, domain.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return legacyCaptureSource{}, nil, err
	}
	if err := lockDocRangeJob(ctx, transaction, job); err != nil {
		return legacyCaptureSource{}, nil, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return legacyCaptureSource{}, nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyCaptureSource{}, nil, err
	}
	if witness.bytes < 0 || len(body) != length {
		return legacyCaptureSource{}, nil, domain.ErrIntegrity
	}
	return witness, body, nil
}

func (storage *PostgresStore) newLegacyCaptureWork(ctx context.Context, job domain.DocRangeIndexJob) (_ *preparedLegacyCaptureWork, result error) {
	witness, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	var work *preparedLegacyCaptureWork
	defer func() {
		if result != nil {
			if work != nil {
				result = errors.Join(result, work.Close())
			}
			result = storage.confirmLegacyCaptureFailure(ctx, job, witness.stage, result)
		}
	}()
	job.RepoID, job.DocHash = domain.ContentHash(strings.Clone(string(job.RepoID))), domain.ContentHash(strings.Clone(string(job.DocHash)))
	job.State, job.Reason, job.BaseHash = strings.Clone(job.State), strings.Clone(job.Reason), domain.ContentHash(strings.Clone(string(job.BaseHash)))
	job.RepositoryEpoch = strings.Clone(job.RepositoryEpoch)
	witness.stage.sourceID = strings.Clone(witness.stage.sourceID)
	work = &preparedLegacyCaptureWork{store: storage, job: job, witness: witness}
	restored, err := work.restoreNormalization(ctx)
	if err != nil {
		return nil, err
	}
	if !restored {
		work.source, err = newLegacyDocSpool()
		if err != nil {
			return nil, err
		}
	}
	if _, err := work.RetainedResources(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

func (work *preparedLegacyCaptureWork) matches(job domain.DocRangeIndexJob) error {
	if work == nil || work.closed || work.source == nil && work.work == nil && work.decoding == nil && work.normalization == nil && !work.canonicalProof.Valid() {
		return os.ErrClosed
	}
	if work.store == nil || job.Validate() != nil || job.State != "running" || job.BaseHash != "" ||
		job.RepoID != work.job.RepoID || job.DocHash != work.job.DocHash || job.Version != work.job.Version || job.RepositoryEpoch != work.job.RepositoryEpoch {
		return domain.ErrConflict
	}
	return nil
}

func (work *preparedLegacyCaptureWork) prepare(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	raw, preparationErr := work.decoding.Bytes(ctx)
	var session legacyByteBackfillSession
	if preparationErr == nil {
		session, preparationErr = work.store.prepareLegacyDecodedByteSnapshot(ctx, job, work.witness.stage, raw)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
		return 0, err
	}
	if errors.Is(preparationErr, domain.ErrDocByteRangeBackfillRequired) {
		work.compatibility = true
		return outbound.DocRangeWorkCompatibilityRequired, nil
	}
	if preparationErr != nil {
		return 0, preparationErr
	}
	prepared, err := work.store.newLegacyByteWork(ctx, job, session)
	if err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
	}
	cleanup := work.closeInputs()
	if work.proofData != nil {
		cleanup = errors.Join(cleanup, work.proofData.Close())
	}
	if cleanup != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, errors.Join(cleanup, prepared.Close()))
	}
	work.work = prepared
	work.source, work.parts, work.witness = nil, nil, legacyCaptureSource{}
	work.decoding, work.normalization = nil, nil
	work.canonical, work.proofData = nil, nil
	work.canonicalProof = domain.VerifiedLegacyByteSource{}
	work.partHeap = rootPublicationHeapEstimate{}
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyCaptureWork) closeInputs() error {
	var failure error
	if work.source != nil {
		failure = work.source.Close()
		work.source = nil
	}
	if work.decoding != nil {
		failure = errors.Join(failure, work.decoding.Close())
		work.decoding = nil
	}
	if work.normalization != nil {
		failure = errors.Join(failure, work.normalization.Close())
		work.normalization = nil
	}
	if failure != nil {
		failure = errors.Join(domain.ErrPreparedDocRangeObject, failure)
	}
	return failure
}

func (work *preparedLegacyCaptureWork) handoffCanonical(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
		return 0, err
	}
	prepared, err := work.store.newLegacySourceByteWork(ctx, job, work.witness.stage, work.canonicalProof)
	if err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
	}
	work.canonical, work.proofData = nil, nil
	work.canonicalProof = domain.VerifiedLegacyByteSource{}
	if err := work.closeInputs(); err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, errors.Join(err, prepared.Close()))
	}
	work.work, work.proofData = prepared, nil
	work.source, work.parts, work.decoding, work.canonical = nil, nil, nil, nil
	work.normalization = nil
	work.witness, work.canonicalProof = legacyCaptureSource{}, domain.VerifiedLegacyByteSource{}
	work.partHeap = rootPublicationHeapEstimate{}
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyCaptureWork) advanceCanonical(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
		return 0, err
	}
	if work.canonical == nil {
		source, err := work.decoding.proofSource(ctx)
		if err != nil {
			return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
		}
		data, err := newLegacyDocSpool()
		if err != nil {
			return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
		}
		preparation, err := domain.NewLegacyByteSourcePreparation(ctx, job.DocHash, source, &legacyProofSpool{legacyDocSpool: data}, &work.store.docProofs.rootVerifier)
		if err != nil {
			return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, errors.Join(err, data.Close()))
		}
		work.proofData, work.canonical = data, preparation
	}
	progress, err := work.canonical.Step(ctx)
	if err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
	}
	switch progress {
	case domain.LegacyByteSourcePending:
	case domain.LegacyByteSourceOriginalRequired:
		normalize := work.canonical.NormalizationEligible()
		if err := work.proofData.Close(); err != nil {
			return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, errors.Join(domain.ErrPreparedDocRangeObject, err))
		}
		work.canonical, work.proofData = nil, nil
		if normalize {
			source, err := work.decoding.normalizationSource(ctx)
			if err != nil {
				return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
			}
			work.normalization, err = newLegacyNormalizationWork(ctx, job.DocHash, source, &work.store.docProofs.rootVerifier)
			if err != nil {
				return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
			}
		} else {
			work.original = true
		}
	case domain.LegacyByteSourceVerified:
		work.canonicalProof, err = work.canonical.Result()
		if err != nil {
			return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
		}
	default:
		return 0, domain.ErrIntegrity
	}
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyCaptureWork) advanceNormalization(ctx context.Context, job domain.DocRangeIndexJob) (progress outbound.DocRangeWorkProgress, failure error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	checkpointBatch := work.checkpoint != nil && work.checkpoint.restoreFailure == nil &&
		(work.checkpoint.restoring && work.checkpoint.restore != nil ||
			work.checkpoint.capture != nil && !work.checkpoint.reclaim && work.checkpoint.position == 14 &&
				work.checkpoint.pending.hash == "" && work.checkpoint.next.Parser != nil)
	if !checkpointBatch {
		if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return 0, canceled
			}
			return 0, err
		}
	}
	defer func() {
		if failure != nil {
			failure = work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, failure)
		}
		if err := ctx.Err(); err != nil {
			progress, failure = 0, err
		}
	}()
	if work.checkpoint.busy() {
		if work.checkpoint.restoring || work.checkpoint.restoreFailure != nil {
			_, err := work.checkpoint.restoreStep(ctx, work.normalization)
			return outbound.DocRangeWorkPending, err
		}
		acknowledged, err := work.checkpoint.write(ctx, work.normalization.storage)
		if err != nil {
			return 0, err
		}
		if acknowledged {
			work.parserSteps = 0
			work.encoderSteps = 0
			work.encoderGeneration = work.checkpoint.root.State.Files[0].Generation
		}
		if acknowledged && work.decoding != nil && !work.checkpoint.root.State.InputPending() {
			if err := work.decoding.Close(); err != nil {
				return 0, errors.Join(domain.ErrPreparedDocRangeObject, err)
			}
			work.decoding = nil
		}
		return outbound.DocRangeWorkPending, nil
	}
	parsing := false
	if work.normalization.canonical == nil && work.normalization.preparation != nil {
		_, err := work.normalization.preparation.ParserInputPosition(ctx)
		if err != nil && !errors.Is(err, domain.ErrDocRangeIndexPending) && !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
			return 0, err
		}
		parsing = err == nil
	}
	complete, err := work.normalization.Step(ctx)
	if errors.Is(err, domain.ErrLegacyNormalizationBusy) {
		return outbound.DocRangeWorkPending, nil
	}
	if err != nil {
		if work.checkpoint != nil && work.checkpoint.recovered && work.normalization.prepared &&
			(errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrPreparedDocRangeObject)) && ctx.Err() == nil {
			work.checkpoint.restoreFailure = err
			return outbound.DocRangeWorkPending, nil
		}
		return 0, err
	}
	if work.normalization.canonical == nil {
		consumed, err := work.normalization.inputConsumed()
		if err != nil {
			return 0, err
		}
		parserCheckpoint := false
		encoderCheckpoint := false
		if !consumed {
			if parsing {
				work.parserSteps = min(work.parserSteps+1, legacyNormalizationParserCheckpointQuanta)
			}
			position, err := work.normalization.preparation.ParserInputPosition(ctx)
			if err != nil && !errors.Is(err, domain.ErrDocRangeIndexPending) && !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
				return 0, err
			}
			if err == nil {
				anchor := 0
				if work.checkpoint != nil {
					anchor = work.checkpoint.parserPosition
				}
				parserCheckpoint = position-anchor >= domain.ConversationManifestChunkBytes || work.parserSteps >= legacyNormalizationParserCheckpointQuanta
				if !parserCheckpoint && (work.checkpoint == nil || work.checkpoint.state.hash == "") {
					resources, err := work.normalization.RetainedResources()
					if err != nil {
						return 0, err
					}
					parserCheckpoint = resources.HeapBytes >= domain.ConversationManifestChunkBytes
				}
			}
			output, err := work.normalization.preparation.UnconsumedOutputPosition(ctx)
			if err != nil && !errors.Is(err, domain.ErrDocRangeIndexPending) && !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
				return 0, err
			}
			if err == nil {
				anchor := domain.LegacyNormalizationOutputProgress{}
				if work.checkpoint != nil {
					file := work.checkpoint.root.State.Files[0]
					anchor.Generation, anchor.Bytes = file.Generation, file.Size
				}
				if output.Generation < anchor.Generation || output.Generation == anchor.Generation && output.Bytes < anchor.Bytes {
					return 0, domain.ErrIntegrity
				}
				if output.Generation != anchor.Generation {
					anchor.Bytes = 0
				}
				if work.encoderGeneration != output.Generation {
					work.encoderGeneration, work.encoderSteps = output.Generation, 0
				}
				if !parsing {
					work.encoderSteps = min(work.encoderSteps+1, legacyNormalizationParserCheckpointQuanta)
				}
				encoderCheckpoint = output.Bytes-anchor.Bytes >= domain.ConversationManifestChunkBytes || work.encoderSteps >= legacyNormalizationParserCheckpointQuanta
			}
		}
		if consumed || parserCheckpoint || encoderCheckpoint {
			if work.checkpoint == nil {
				stage, err := work.store.ensureLegacyNormalizationStage(ctx, job, work.witness.stage)
				if err != nil {
					return 0, err
				}
				work.witness.stage = stage
				state, _, err := work.store.loadLegacyNormalizationCheckpoint(ctx, job)
				if err != nil {
					return 0, err
				}
				if state.stage.plan != "" && state.hash == "" {
					if err := work.store.discardLegacyByteRecovery(ctx, job, state.stage); err != nil {
						return 0, err
					}
					source := work.witness.stage
					source.incarnation = ""
					stage, err := work.store.ensureLegacyNormalizationStage(ctx, job, source)
					if err != nil {
						return 0, err
					}
					work.witness.stage = stage
					state, _, err = work.store.loadLegacyNormalizationCheckpoint(ctx, job)
					if err != nil {
						return 0, err
					}
				}
				work.checkpoint = &legacyNormalizationCheckpointWork{store: work.store, job: work.job, state: state}
			}
			if err := work.checkpoint.begin(ctx, work.normalization.preparation); err != nil {
				if errors.Is(err, domain.ErrLegacyNormalizationBusy) || errors.Is(err, domain.ErrDocRangeIndexPending) {
					return outbound.DocRangeWorkPending, nil
				}
				return 0, err
			}
		}
	}
	if !complete {
		return outbound.DocRangeWorkPending, nil
	}
	proof, err := work.normalization.canonical.Result()
	if err != nil {
		return 0, err
	}
	work.canonicalProof = proof
	work.canonical, work.proofData = work.normalization.canonical, work.normalization.data
	work.normalization.canonical, work.normalization.data = nil, nil
	if err := work.normalization.Close(); err != nil {
		return 0, err
	}
	work.normalization = nil
	return outbound.DocRangeWorkPending, nil
}

func (storage *PostgresStore) confirmLegacyCaptureFailure(ctx context.Context, job domain.DocRangeIndexJob, stage legacyByteStage, failure error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.confirmLegacyBytePreparation(ctx, job, stage); err != nil {
		return err
	}
	return failure
}

func (work *preparedLegacyCaptureWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := work.matches(job); err != nil {
		return 0, err
	}
	if work.work != nil {
		progress, err := work.work.Step(ctx, job)
		if err == nil && progress == outbound.DocRangeWorkPublishable && work.checkpoint != nil {
			if err := work.store.discardLegacyNormalizationCheckpoint(ctx, job, work.checkpoint.state); err != nil {
				return 0, err
			}
			work.checkpoint.Close()
			work.checkpoint = nil
		}
		return progress, err
	}
	if work.compatibility || work.witness.bytes > 2*maxLegacyReadBytes {
		if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
			return 0, err
		}
		if work.compatibility {
			return outbound.DocRangeWorkCompatibilityRequired, nil
		}
		return 0, domain.ErrDocumentResourceLimit
	}
	if work.normalization != nil {
		return work.advanceNormalization(ctx, job)
	}
	if work.canonicalProof.Valid() {
		return work.handoffCanonical(ctx, job)
	}
	if work.source == nil || work.source.size == work.witness.bytes {
		if work.decoding == nil || !work.decoding.complete {
			if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
				return 0, err
			}
			if work.decoding == nil {
				var err error
				work.decoding, err = newLegacyCaptureDecodeWork(ctx, work.source, work.parts, work.witness.bytes)
				if err != nil {
					return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
				}
			}
			if _, err := work.decoding.Step(ctx); err != nil {
				return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
			}
			return outbound.DocRangeWorkPending, nil
		}
		if work.source != nil {
			if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
				return 0, err
			}
			if err := work.source.Close(); err != nil {
				return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
			}
			work.source, work.parts = nil, nil
			work.partHeap = rootPublicationHeapEstimate{}
			return outbound.DocRangeWorkPending, nil
		}
		if work.original {
			return work.prepare(ctx, job)
		}
		return work.advanceCanonical(ctx, job)
	}
	if work.source.size < 0 || work.source.size > work.witness.bytes {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, domain.ErrPreparedDocRangeObject)
	}
	length := min(domain.ConversationManifestChunkBytes, work.witness.bytes-work.source.size)
	_, body, err := work.store.readLegacyCaptureSource(ctx, job, &work.witness, work.source.size, length)
	if err != nil {
		return 0, err
	}
	hash, err := domain.HashContentContext(ctx, body)
	if err != nil {
		return 0, err
	}
	if err := work.source.Append(ctx, body); err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, err)
	}
	work.partHeap.allocation(int64(len(hash)), 1)
	if work.partHeap.err != nil {
		return 0, work.store.confirmLegacyCaptureFailure(ctx, job, work.witness.stage, work.partHeap.err)
	}
	work.parts = append(work.parts, legacyCapturePart{hash: hash, bytes: len(body)})
	return outbound.DocRangeWorkPending, nil
}

func (work *preparedLegacyCaptureWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.matches(job); err != nil {
		return err
	}
	if work.work == nil {
		return domain.ErrDocRangeIndexPending
	}
	return work.work.Publish(ctx, job)
}

func (work *preparedLegacyCaptureWork) RetainedResources() (outbound.RootPublicationResources, error) {
	if work == nil {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	if err := work.matches(work.job); err != nil {
		return outbound.RootPublicationResources{}, err
	}
	var resources outbound.RootPublicationResources
	if work.work != nil {
		var err error
		resources, err = work.work.RetainedResources()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
	}
	if work.decoding != nil {
		var err error
		resources, err = work.decoding.RetainedResources()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
	}
	if work.canonical != nil {
		retained, err := work.canonical.RetainedBytes()
		if err != nil || retained < 0 || resources.HeapBytes > math.MaxInt64-retained {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrIntegrity, err)
		}
		resources.HeapBytes += retained
	}
	if work.normalization != nil {
		retained, err := work.normalization.RetainedResources()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
		if retained.HeapBytes < 0 || retained.SpoolBytes < 0 || resources.HeapBytes > math.MaxInt64-retained.HeapBytes ||
			resources.SpoolBytes > math.MaxInt64-retained.SpoolBytes {
			return outbound.RootPublicationResources{}, domain.ErrIntegrity
		}
		resources.HeapBytes += retained.HeapBytes
		resources.SpoolBytes += retained.SpoolBytes
	}
	if work.checkpoint != nil {
		retained, err := work.checkpoint.retainedBytes()
		if err != nil || retained < 0 || resources.HeapBytes > math.MaxInt64-retained {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrIntegrity, err)
		}
		resources.HeapBytes += retained
	}
	if work.proofData != nil {
		info, err := work.proofData.file.Stat()
		if err != nil || info.Size() != int64(work.proofData.size) || info.Size() < 0 || resources.SpoolBytes > math.MaxInt64-info.Size() {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
		}
		resources.SpoolBytes += info.Size()
	}
	if work.partHeap.err != nil || work.partHeap.bytes < 0 || resources.HeapBytes > math.MaxInt64-work.partHeap.bytes {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	estimate := rootPublicationHeapEstimate{bytes: resources.HeapBytes + work.partHeap.bytes}
	estimate.allocation(1, 8192+int64(unsafe.Sizeof(*work)))
	estimate.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(legacyCapturePart{})))
	if work.proofData != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*work.proofData)+unsafe.Sizeof(legacyProofSpool{})))
		estimate.allocation(int64(len(work.proofData.file.Name())), 1)
	}
	for _, value := range []string{string(work.job.RepoID), string(work.job.DocHash), string(work.job.BaseHash), work.job.State, work.job.Reason, work.job.RepositoryEpoch, work.witness.stage.sourceID} {
		estimate.allocation(int64(len(value)), 1)
	}
	if work.source != nil {
		info, err := work.source.file.Stat()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
		if info.Size() < 0 || info.Size() != int64(work.source.size) || work.source.size > work.witness.bytes ||
			resources.SpoolBytes > math.MaxInt64-info.Size() {
			return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
		}
		resources.SpoolBytes += info.Size()
		estimate.allocation(1, int64(unsafe.Sizeof(*work.source)))
		estimate.allocation(int64(len(work.source.file.Name())), 1)
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	resources.HeapBytes = estimate.bytes
	return resources, nil
}

func (work *preparedLegacyCaptureWork) Close() error {
	if work == nil {
		return nil
	}
	work.once.Do(func() {
		work.closed = true
		if work.checkpoint != nil {
			work.checkpoint.Close()
			work.checkpoint = nil
		}
		work.err = work.closeInputs()
		if work.work != nil {
			if err := work.work.Close(); err != nil {
				work.err = errors.Join(work.err, domain.ErrPreparedDocRangeObject, err)
			}
		}
		if work.proofData != nil {
			if err := work.proofData.Close(); err != nil {
				work.err = errors.Join(work.err, domain.ErrPreparedDocRangeObject, err)
			}
		}
		work.source, work.parts, work.work = nil, nil, nil
		work.decoding, work.normalization = nil, nil
		work.canonical, work.proofData = nil, nil
		work.canonicalProof = domain.VerifiedLegacyByteSource{}
		work.job, work.witness = domain.DocRangeIndexJob{}, legacyCaptureSource{}
		work.partHeap = rootPublicationHeapEstimate{}
	})
	return work.err
}

var _ outbound.PreparedDocRangeWork = (*preparedLegacyCaptureWork)(nil)
