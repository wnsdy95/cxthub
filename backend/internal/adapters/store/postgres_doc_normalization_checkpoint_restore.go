//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (work *preparedLegacyCaptureWork) restoreNormalization(ctx context.Context) (bool, error) {
	state, found, err := work.store.loadLegacyNormalizationCheckpoint(ctx, work.job)
	if err != nil || !found {
		return false, err
	}
	if state.stage.sourceID != work.witness.stage.sourceID || state.stage.sourceVersion != work.witness.stage.sourceVersion {
		return false, domain.ErrConflict
	}
	checkpoint := &legacyNormalizationCheckpointWork{store: work.store, job: work.job, state: state, restoring: true, recovered: true}
	work.checkpoint, work.witness.stage = checkpoint, state.stage
	body, err := work.store.readLegacyNormalizationCheckpointObject(ctx, work.job, state, state.hash, state.bytes)
	if err != nil {
		return false, checkpoint.fail(ctx, err)
	}
	checkpoint.root, err = decodeLegacyNormalizationCheckpoint(body, state)
	if err != nil {
		return false, checkpoint.fail(ctx, err)
	}
	storage, err := newLegacyNormalizationSpool(ctx)
	if err != nil {
		return false, err
	}
	work.normalization = &legacyNormalizationWork{want: work.job.DocHash, verifier: domain.NewCanonicalDocVerifierWithoutCache(),
		storage: storage, verifierOwned: true}
	return true, nil
}

func (work *legacyNormalizationCheckpointWork) readHeader(ctx context.Context, offset, length int) ([]byte, error) {
	return work.store.readLegacyNormalizationPrefix(ctx, work.job, work.state, work.root.Header, offset, length)
}

func (work *legacyNormalizationCheckpointWork) readBuffer(ctx context.Context, slot int) ([]byte, error) {
	if slot < 0 || slot >= len(work.root.Files) {
		return nil, domain.ErrIntegrity
	}
	buffer := work.root.Files[slot].Buffer
	if buffer.Bytes == 0 {
		return nil, ctx.Err()
	}
	return work.store.readLegacyNormalizationCheckpointObject(ctx, work.job, work.state, buffer.Hash, buffer.Bytes)
}

func (work *legacyNormalizationCheckpointWork) readParts(ctx context.Context, slot, first, count int) ([]domain.ConversationManifestChunk, error) {
	if slot < 0 || slot >= len(work.root.Files) || first < 0 || count < 0 || count > 32 ||
		first > work.root.State.Files[slot].Parts || count > work.root.State.Files[slot].Parts-first {
		return nil, domain.ErrIntegrity
	}
	body, err := work.store.readLegacyNormalizationPrefix(ctx, work.job, work.state, work.root.Files[slot].Parts,
		first*legacyNormalizationReceiptBytes, count*legacyNormalizationReceiptBytes)
	if err != nil {
		return nil, err
	}
	parts := make([]domain.ConversationManifestChunk, count)
	for ordinal := range parts {
		position := ordinal * legacyNormalizationReceiptBytes
		parts[ordinal] = domain.ConversationManifestChunk{Bytes: int64(binary.BigEndian.Uint32(body[position:])),
			Hash: domain.ContentHash("sha256:" + hex.EncodeToString(body[position+4:position+legacyNormalizationReceiptBytes]))}
	}
	return parts, ctx.Err()
}

func (work *legacyNormalizationCheckpointWork) fail(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.store.confirmLegacyBytePreparation(ctx, work.job, work.state.stage); err != nil {
		return err
	}
	if errors.Is(cause, domain.ErrConflict) || errors.Is(cause, domain.ErrNotFound) || errors.Is(cause, errLegacyByteRecoveryLocal) {
		return cause
	}
	if errors.Is(cause, domain.ErrPreparedDocRangeObject) || errors.Is(cause, domain.ErrIntegrity) {
		if err := work.store.discardLegacyNormalizationCheckpoint(ctx, work.job, work.state); err != nil {
			return err
		}
		if work.state.stage.plan != "" {
			if err := work.store.discardLegacyByteRecovery(ctx, work.job, work.state.stage); err != nil {
				return err
			}
		}
		return errors.Join(domain.ErrPreparedDocRangeObject, cause)
	}
	return cause
}

func (work *legacyNormalizationCheckpointWork) audit(ctx context.Context, storage *legacyNormalizationSpool) (bool, error) {
	if work.auditSlot == domain.LegacyNormalizationSlots {
		return false, work.fail(ctx, work.restoreFailure)
	}
	prefix := work.root.Files[work.auditSlot].Data
	if work.root.State.Ready() && work.auditSlot < 3 {
		work.auditSlot++
		work.auditOffset = 0
		return false, nil
	}
	spool, err := storage.slot(ctx, work.auditSlot)
	if err != nil || spool.size != prefix.Tree.DataBytes {
		return false, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
	}
	if work.auditOffset == prefix.Tree.DataBytes {
		work.auditSlot++
		work.auditOffset = 0
		return false, nil
	}
	length := min(domain.LegacyCanonicalEncodingBytes, prefix.Tree.DataBytes-work.auditOffset)
	expected, err := work.store.readLegacyNormalizationPrefix(ctx, work.job, work.state, prefix, work.auditOffset, length)
	if err != nil {
		return false, work.fail(ctx, err)
	}
	actual, err := storage.Read(ctx, work.auditSlot, work.auditOffset, length)
	if err != nil || !bytes.Equal(expected, actual) {
		return false, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
	}
	work.auditOffset += length
	return false, nil
}

func (work *legacyNormalizationCheckpointWork) restoreStep(ctx context.Context, normalization *legacyNormalizationWork) (bool, error) {
	if work.restoreFailure != nil {
		return work.audit(ctx, normalization.storage)
	}
	if work.restoreSlot < domain.LegacyNormalizationSlots {
		prefix := work.root.Files[work.restoreSlot].Data
		spool, err := normalization.storage.slot(ctx, work.restoreSlot)
		if err != nil {
			return false, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
		}
		if spool.size > prefix.Tree.DataBytes {
			return false, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal)
		}
		if spool.size == prefix.Tree.DataBytes {
			work.restoreSlot++
			return false, nil
		}
		length := min(domain.LegacyCanonicalEncodingBytes, prefix.Tree.DataBytes-spool.size)
		body, err := work.store.readLegacyNormalizationPrefix(ctx, work.job, work.state, prefix, spool.size, length)
		if err != nil {
			return false, work.fail(ctx, err)
		}
		if err := normalization.storage.Append(ctx, work.restoreSlot, body); err != nil {
			return false, errors.Join(domain.ErrPreparedDocRangeObject, errLegacyByteRecoveryLocal, err)
		}
		return false, nil
	}
	if work.restore == nil {
		source := domain.LegacyNormalizationCheckpointSource{
			Header:     domain.LegacyByteSource{Bytes: work.root.State.HeaderBytes, Read: work.readHeader},
			ReadBuffer: work.readBuffer, ReadParts: work.readParts}
		if work.root.Parser != nil {
			ready, err := work.restoreParserParts(ctx)
			if err != nil {
				return false, work.fail(ctx, err)
			}
			if !ready {
				return false, nil
			}
			parser := work.root.Parser
			source.Parser = &domain.LegacyNormalizationParserCheckpointSource{
				Ref:     domain.LegacyNormalizationParserCheckpointRef{Hash: parser.Ref.Hash, Bytes: parser.Ref.Bytes},
				Binding: work.parserBinding(), Store: work.parserIO(), Budget: legacyNormalizationParserHeapBudget,
				Source: domain.LegacyByteSource{Bytes: parser.Input.Tree.DataBytes, PartBytes: parser.Input.ChunkBytes,
					Parts: work.parserParts, Read: work.readParserInput},
			}
		}
		if work.root.Encoder != nil {
			encoder := work.root.Encoder
			source.Encoder = &domain.LegacyNormalizationParserCheckpointSource{
				Ref:     domain.LegacyNormalizationParserCheckpointRef{Hash: encoder.Hash, Bytes: encoder.Bytes},
				Binding: work.parserBinding(), Store: work.parserIO(), Budget: legacyNormalizationParserHeapBudget,
			}
		}
		restore, err := domain.NewLegacyNormalizationRestore(ctx, work.root.State, source, normalization.storage)
		if err != nil {
			return false, work.fail(ctx, err)
		}
		work.restore = restore
		return false, nil
	}
	complete, err := work.restore.Step(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrLegacyNormalizationBusy) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) || ctx.Err() != nil {
			return false, err
		}
		work.restore.Close()
		work.restore, work.restoreFailure = nil, err
		return false, nil
	}
	if !complete {
		return false, nil
	}
	preparation, err := work.restore.Result()
	if err != nil {
		return false, err
	}
	work.restore, work.restoring = nil, false
	work.parserParts = nil
	normalization.preparation = preparation
	normalization.prepared = work.root.State.Ready()
	if work.root.State.InputPending() {
		work.parserPosition, err = preparation.UnconsumedInputPosition(ctx)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}
