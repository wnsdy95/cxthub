package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"unsafe"
)

type LegacyNormalizationParserCheckpointRef struct {
	Hash  ContentHash
	Bytes int
}

type LegacyNormalizationParserCheckpointIO struct {
	Put  func(context.Context, ContentHash, []byte) error
	Read func(context.Context, ContentHash, int) ([]byte, error)
}

type LegacyNormalizationParserCheckpointSource struct {
	Ref     LegacyNormalizationParserCheckpointRef
	Binding ContentHash
	Source  LegacyByteSource
	Store   LegacyNormalizationParserCheckpointIO
	Budget  int64
}

func (work *LegacyNormalizationPreparation) ParserInputPosition(ctx context.Context) (int, error) {
	return work.inputPosition(ctx, false)
}

func (work *LegacyNormalizationPreparation) UnconsumedInputPosition(ctx context.Context) (int, error) {
	return work.inputPosition(ctx, true)
}

func (work *LegacyNormalizationPreparation) inputPosition(ctx context.Context, staging bool) (int, error) {
	input, _, err := work.pendingPositions(ctx, staging)
	return input, err
}

type LegacyNormalizationOutputProgress struct {
	Generation uint64
	Bytes      int
}

func (work *LegacyNormalizationPreparation) UnconsumedOutputPosition(ctx context.Context) (LegacyNormalizationOutputProgress, error) {
	_, output, err := work.pendingPositions(ctx, true)
	return output, err
}

func (work *LegacyNormalizationPreparation) pendingPositions(ctx context.Context, staging bool) (int, LegacyNormalizationOutputProgress, error) {
	if err := ctx.Err(); err != nil {
		return 0, LegacyNormalizationOutputProgress{}, err
	}
	if work == nil || work.mutation == nil {
		return 0, LegacyNormalizationOutputProgress{}, ErrIntegrity
	}
	if !work.mutation.stepping.CompareAndSwap(false, true) {
		return 0, LegacyNormalizationOutputProgress{}, ErrLegacyNormalizationBusy
	}
	defer work.mutation.stepping.Store(false)
	if err := work.mutation.enter(); err != nil {
		return 0, LegacyNormalizationOutputProgress{}, err
	}
	defer work.mutation.leave()
	if work.terminal != nil {
		return 0, LegacyNormalizationOutputProgress{}, work.terminal
	}
	if work.phase != legacyNormalizationDecode && (!staging || work.phase != legacyNormalizationStage) || work.decoder == nil {
		return 0, LegacyNormalizationOutputProgress{}, ErrDocRangeIndexPending
	}
	if work.decoder.position < 0 || work.decoder.position > work.decoder.source.Bytes || work.files[0].size < 0 {
		return 0, LegacyNormalizationOutputProgress{}, ErrIntegrity
	}
	return work.decoder.position, LegacyNormalizationOutputProgress{Generation: work.files[0].generation, Bytes: work.files[0].size}, nil
}

func (capture *LegacyNormalizationCheckpointCapture) ParserInputPosition(ctx context.Context) (int, error) {
	if err := capture.valid(ctx); err != nil {
		return 0, err
	}
	if !capture.control.InputPending() || capture.work.decoder == nil {
		return 0, ErrDocRangeIndexPending
	}
	return capture.work.decoder.position, nil
}

func (control LegacyNormalizationControl) validateParser() error {
	if control.Phase != legacyNormalizationDecode || control.HeaderBytes != 0 || control.PrefixBytes != 0 || control.Written != 0 ||
		len(control.HashState) != 0 || control.Active != (LegacyNormalizationRecordControl{}) || control.Position != 0 ||
		control.SourceSlot != 0 || control.TargetSlot != 0 || control.Width != 0 || control.Base != 0 || control.Left != 0 || control.Right != 0 ||
		control.Segment != 0 || control.SegmentPos != 0 || control.Verified != 0 || control.Files[1].Size != control.Count*legacyNormalizationRecordBytes ||
		control.Files[2].Size != 0 || control.Files[3].Size != 0 || control.Count == 0 && control.Files[0].Size != 0 || control.Files[0].Size < control.Count {
		return ErrIntegrity
	}
	return nil
}

func legacyNormalizationParserCount(decoder *LegacyNormalizationDecoder, count int) bool {
	return decoder != nil && decoder.count >= count && (decoder.count == count || decoder.invalid)
}

func (capture *LegacyNormalizationCheckpointCapture) ParserInputGeometry(ctx context.Context) (int, int, error) {
	if err := capture.valid(ctx); err != nil {
		return 0, 0, err
	}
	if !capture.control.InputPending() || capture.work.decoder == nil {
		return 0, 0, ErrDocRangeIndexPending
	}
	source := capture.work.decoder.source
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return 0, 0, err
	}
	return source.Bytes, source.partBytes(), nil
}

func (capture *LegacyNormalizationCheckpointCapture) ReadParserInput(ctx context.Context, offset, length int) ([]byte, error) {
	size, _, err := capture.ParserInputGeometry(ctx)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > LegacyCanonicalEncodingBytes || offset > size || length > size-offset {
		return nil, ErrIntegrity
	}
	body, err := capture.work.decoder.source.Read(ctx, offset, length)
	if current := capture.valid(ctx); current != nil {
		return nil, current
	}
	if err != nil || len(body) != length {
		return nil, errors.Join(ErrPreparedDocRangeObject, err)
	}
	return bytes.Clone(body), ctx.Err()
}

func (capture *LegacyNormalizationCheckpointCapture) ParserStep(ctx context.Context, binding ContentHash, store LegacyNormalizationParserCheckpointIO, budget int64) (ref LegacyNormalizationParserCheckpointRef, complete bool, failure error) {
	if capture == nil {
		return ref, false, ErrConflict
	}
	if !capture.parserBusy.CompareAndSwap(false, true) {
		return ref, false, ErrLegacyNormalizationBusy
	}
	defer func() {
		if failure != nil {
			capture.closed.Store(true)
		}
		if capture.closed.Load() {
			capture.release()
		}
		capture.parserBusy.Store(false)
		if capture.closed.Load() && capture.parserBusy.CompareAndSwap(false, true) {
			capture.release()
			capture.parserBusy.Store(false)
		}
	}()
	if err := capture.valid(ctx); err != nil {
		return ref, false, err
	}
	if !capture.control.InputPending() {
		return ref, true, nil
	}
	if ValidateContentHash(binding) != nil || budget <= 0 || store.Put == nil ||
		capture.parserBudget != 0 && (capture.parserBudget != budget || capture.parserBinding != binding) {
		return ref, false, ErrIntegrity
	}
	if capture.parserRef != (LegacyNormalizationParserCheckpointRef{}) {
		return capture.parserRef, true, nil
	}
	if capture.parser == nil {
		retained, err := capture.work.RetainedBytes()
		if err != nil {
			return ref, false, err
		}
		extra, err := capture.RetainedBytes()
		if err != nil {
			return ref, false, err
		}
		var planned docRangeHeaderHeapEstimate
		planned.include(docRangeHeaderHeapEstimate{bytes: retained})
		planned.include(docRangeHeaderHeapEstimate{bytes: extra})
		planned.allocation(int64(len(binding)), 1)
		if planned.err != nil || planned.bytes > budget {
			return ref, false, ErrDocumentResourceLimit
		}
		shared, err := capture.work.decoder.RetainedBytes()
		if err != nil {
			return ref, false, err
		}
		var grant docRangeHeaderHeapEstimate
		grant.allocation(1, int64(unsafe.Sizeof(*capture.capture)))
		childBudget := docRangeHeaderHeapEstimate{bytes: budget - planned.bytes}
		childBudget.include(docRangeHeaderHeapEstimate{bytes: shared})
		childBudget.include(grant)
		if childBudget.err != nil {
			return ref, false, childBudget.err
		}
		capture.parser, err = newLegacyNormalizationDecoderCheckpoint(ctx, capture.work.decoder, binding,
			legacyNativeCheckpointIO{Put: store.Put, Read: store.Read}, childBudget.bytes)
		if err != nil {
			return ref, false, err
		}
		capture.parserBinding = ContentHash(strings.Clone(string(binding)))
		capture.parserBudget = budget
	}
	ready, err := capture.parser.step(ctx)
	if current := capture.valid(ctx); current != nil {
		return ref, false, current
	}
	if err != nil || !ready {
		return ref, false, err
	}
	root, err := capture.parser.take(ctx)
	capture.parser = nil
	if err != nil {
		return ref, false, err
	}
	capture.parserRef = LegacyNormalizationParserCheckpointRef{Hash: root.Hash, Bytes: root.Bytes}
	return capture.parserRef, true, nil
}

func (restore *LegacyNormalizationRestore) restoreParser(ctx context.Context) (bool, error) {
	retained, err := restore.retainedBytes()
	if err != nil || retained > restore.parserBudget {
		if err == nil {
			err = ErrDocumentResourceLimit
		}
		return false, err
	}
	childBytes, err := restore.parser.retainedBytes()
	if err != nil || childBytes > retained {
		if err == nil {
			err = ErrIntegrity
		}
		return false, err
	}
	restore.parser.budget = restore.parserBudget - (retained - childBytes)
	complete, err := restore.parser.step(ctx)
	if current := restore.current(ctx); current != nil {
		return false, current
	}
	if err != nil || !complete {
		return false, err
	}
	decoder, err := restore.parser.take(ctx)
	restore.parser = nil
	if err != nil {
		return false, err
	}
	if !legacyNormalizationParserCount(decoder, restore.control.Count+restore.control.StagePending) {
		decoder.mutation.stop()
		return false, ErrIntegrity
	}
	restore.work.decoder, restore.work.mutation = decoder, decoder.mutation
	return true, nil
}

func (restore *LegacyNormalizationRestore) parserAdmission(extra docRangeHeaderHeapEstimate) error {
	if restore.parserBudget == 0 {
		return nil
	}
	retained, err := restore.retainedBytes()
	if err != nil {
		return err
	}
	extra.include(docRangeHeaderHeapEstimate{bytes: retained})
	if restore.parser != nil || restore.stage != nil {
		extra.allocation(legacyNativeAnyCheckpointScratch, 1)
	}
	if extra.err != nil || extra.bytes > restore.parserBudget {
		return ErrDocumentResourceLimit
	}
	return nil
}
