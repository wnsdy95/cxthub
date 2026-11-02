package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unsafe"
)

const legacyNativeCheckpointTypedNumberMetadataBytes = 8192

type legacyNativeCheckpointTypedNumberState struct {
	Version      uint8                         `json:"version"`
	SourceBytes  int                           `json:"source_bytes"`
	From         int                           `json:"from"`
	End          int                           `json:"end"`
	Field        int                           `json:"field"`
	Validated    int                           `json:"validated"`
	Mode         legacyJSONValueMode           `json:"mode"`
	Diagnostics  legacyNativeNumberDiagnostics `json:"diagnostics"`
	Quoted       bool                          `json:"quoted"`
	Decoded      bool                          `json:"decoded"`
	Rejected     bool                          `json:"rejected"`
	Complete     bool                          `json:"complete"`
	NewTarget    bool                          `json:"new_target"`
	Semantic     uint8                         `json:"semantic"`
	HasScalar    bool                          `json:"has_scalar"`
	Scalar       legacyNativeCheckpointScalar  `json:"scalar"`
	HasErrorText bool                          `json:"has_error_text"`
	ErrorText    legacyNativeCheckpointScalar  `json:"error_text"`
	Target       legacyNativeCheckpointText    `json:"target"`
	PartBytes    int                           `json:"part_bytes"`
	SourceParts  legacyNativeCheckpointTree    `json:"source_parts"`
}

func legacyNativeCheckpointTypedNumberScalarState(scalar *legacyNativeScalar) (legacyNativeCheckpointScalar, error) {
	if scalar == nil {
		return legacyNativeCheckpointScalar{}, nil
	}
	state := legacyNativeScalarCheckpoint{
		Version: 1, Position: scalar.position, End: scalar.end, Maximum: scalar.maximum,
		Present: scalar.text != nil, Expanded: scalar.expanded,
	}
	accepted := scalar.text
	if scalar.text != nil {
		state.Capacity = scalar.text.Cap()
	}
	if scalar.previous != nil {
		if scalar.text == nil || scalar.previous == scalar.text || scalar.previous.Len() == 0 ||
			scalar.copied < 0 || scalar.copied >= scalar.previous.Len() || scalar.text.Len() != scalar.copied {
			return legacyNativeCheckpointScalar{}, ErrIntegrity
		}
		accepted = scalar.previous
	}
	if accepted != nil {
		state.Bytes = accepted.Len()
	}
	if scalar.buffered < 0 || scalar.buffered > len(scalar.pending) || scalar.buffered > scalar.end-scalar.position {
		return legacyNativeCheckpointScalar{}, ErrIntegrity
	}
	if err := state.validate(MaxFinalizedDocBytes); err != nil {
		return legacyNativeCheckpointScalar{}, err
	}
	return legacyNativeCheckpointScalar{State: state, Text: legacyNativeCheckpointText{Bytes: state.Bytes}}, nil
}

func legacyNativeCheckpointTypedNumberTextValid(text legacyNativeCheckpointText) bool {
	return text.Bytes >= 0 && uint64(text.Bytes) <= legacyNativeValueCheckpointPayloadLimit() &&
		text.Tree.valid() && text.Tree.Count == uint64(text.Bytes) &&
		(text.Bytes == 0) == (text.Tree == (legacyNativeCheckpointTree{}))
}

func (state legacyNativeCheckpointTypedNumberState) needsSource() bool {
	return state.Quoted && state.Diagnostics == legacyNativeNumberDiagnosticsFull && !state.Complete
}

func (state legacyNativeCheckpointTypedNumberState) sourceRange() (int, int) {
	if !state.needsSource() || state.PartBytes == 0 {
		return 0, 0
	}
	from, consumed := state.From, state.End-1
	if state.HasScalar {
		consumed = state.Scalar.State.Position
	}
	if state.HasErrorText {
		from = state.ErrorText.State.Position
	}
	return legacyNativeCheckpointNumberSourceRange(from, consumed, state.SourceBytes, state.PartBytes)
}

func (state legacyNativeCheckpointTypedNumberState) validate(checkTrees bool) error {
	if state.Version != legacyNativeCheckpointVersion || state.SourceBytes <= 0 || state.SourceBytes > MaxFinalizedDocBytes ||
		state.From < 0 || state.End <= state.From || state.End > state.SourceBytes || state.End-state.From <= LegacyCanonicalEncodingBytes ||
		state.End-state.From > (int(^uint(0)>>1)-len(legacyNativeTypedNumberErrorPrefix)-len(legacyNativeTypedNumberErrorSuffix)-2)/6 ||
		state.Field < 0 ||
		state.Diagnostics != legacyNativeNumberDiagnosticsFull && state.Diagnostics != legacyNativeNumberDiagnosticsRejectOnly ||
		state.Mode != legacyJSONValueIdle && (state.Mode < legacyJSONValueMinus || state.Mode > legacyJSONValueExponentDigits) ||
		state.Validated < 0 || (state.Validated == 0) != (state.Mode == legacyJSONValueIdle) ||
		state.Target.Bytes < 0 || uint64(state.Target.Bytes) > legacyNativeValueCheckpointPayloadLimit() ||
		state.NewTarget && state.Target.Bytes != 0 || checkTrees && !legacyNativeCheckpointTypedNumberTextValid(state.Target) {
		return ErrIntegrity
	}
	from, end, maximum := state.From, state.End, state.End-state.From
	if state.Quoted {
		from, end, maximum = from+1, end-1, 3*(maximum-2)
	}
	if state.Validated > maximum || state.HasScalar == state.Rejected ||
		!state.Decoded && (state.Rejected || state.Validated != 0) || state.Complete && !state.Rejected ||
		state.Rejected && !state.Quoted || state.HasErrorText && (!state.Rejected || state.Diagnostics != legacyNativeNumberDiagnosticsFull) ||
		state.Semantic > 1 || (state.Semantic == 1) != (state.Complete && state.Diagnostics == legacyNativeNumberDiagnosticsFull) ||
		state.Complete && state.Diagnostics == legacyNativeNumberDiagnosticsFull && !state.HasErrorText {
		return ErrIntegrity
	}
	if state.HasScalar {
		scalar := state.Scalar.State
		if err := scalar.validate(state.SourceBytes); err != nil {
			return err
		}
		if scalar.Position < from || scalar.End != end || scalar.Maximum != maximum ||
			scalar.Bytes != state.Scalar.Text.Bytes || scalar.Bytes > 3*(scalar.Position-from) ||
			!scalar.Present && (scalar.Position != from || scalar.Expanded) ||
			scalar.Position-from > 6*scalar.Bytes ||
			!scalar.Expanded && scalar.Bytes > scalar.Position-from ||
			!state.Quoted && (scalar.Expanded || scalar.Bytes != scalar.Position-from) ||
			state.Decoded && (!scalar.Present || scalar.Position != scalar.End) || state.Validated > scalar.Bytes ||
			checkTrees && !legacyNativeCheckpointTypedNumberTextValid(state.Scalar.Text) {
			return ErrIntegrity
		}
	} else if state.Scalar != (legacyNativeCheckpointScalar{}) {
		return ErrIntegrity
	}
	if state.HasErrorText {
		scalar := state.ErrorText.State
		prefix := len(legacyNativeTypedNumberErrorPrefix) + 1
		suffix := 0
		if state.Complete {
			suffix = len(legacyNativeTypedNumberErrorSuffix) + 1
		}
		if err := scalar.validate(state.SourceBytes); err != nil {
			return err
		}
		if !scalar.Present || !scalar.Expanded || scalar.Position < state.From || scalar.End != state.End ||
			scalar.Maximum != 6*(state.End-state.From)+prefix+len(legacyNativeTypedNumberErrorSuffix)+1 ||
			scalar.Bytes != state.ErrorText.Text.Bytes || scalar.Bytes < prefix+scalar.Position-state.From+suffix ||
			scalar.Bytes > prefix+6*(scalar.Position-state.From)+suffix || state.Complete && scalar.Position != scalar.End ||
			checkTrees && !legacyNativeCheckpointTypedNumberTextValid(state.ErrorText.Text) {
			return ErrIntegrity
		}
	} else if state.ErrorText != (legacyNativeCheckpointScalar{}) {
		return ErrIntegrity
	}
	if state.needsSource() {
		if state.PartBytes != LegacyNormalizationPartBytes && state.PartBytes != ConversationManifestChunkBytes {
			return ErrIntegrity
		}
		first, limit := state.sourceRange()
		if checkTrees && (!state.SourceParts.valid() || state.SourceParts.Count != uint64(limit-first)) {
			return ErrIntegrity
		}
	} else if state.PartBytes != 0 || state.SourceParts != (legacyNativeCheckpointTree{}) {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeCheckpointTypedNumberWriter struct {
	state      legacyNativeCheckpointTypedNumberState
	io         legacyNativeCheckpointIO
	live       LegacyByteSource
	scalar     *legacyNativeCheckpointScalarWriter
	errorText  *legacyNativeCheckpointScalarWriter
	target     *legacyNativeCheckpointTextWriter
	targetText string
	parts      legacyNativeCheckpointListWriter[legacyNativeCheckpointNumberSourcePart]
	ref        legacyNativeCheckpointRef
	budget     int64
	sourceNext int
	sourceDone bool
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeCheckpointTypedNumberWriter(ctx context.Context, number *legacyNativeTypedNumber, currentSource LegacyByteSource, storage legacyNativeCheckpointIO, heapBudget int64) (*legacyNativeCheckpointTypedNumberWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if number == nil || number.target == nil || storage.Put == nil || heapBudget <= 0 ||
		number.authority != currentSource.authority || number.sourceBytes != currentSource.Bytes {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(currentSource, number.sourceBytes); err != nil {
		return nil, err
	}
	state := legacyNativeCheckpointTypedNumberState{
		Version: legacyNativeCheckpointVersion, SourceBytes: number.sourceBytes,
		From: number.from, End: number.end, Field: number.field, Validated: number.validated,
		Mode: number.mode, Diagnostics: number.diagnostics, Quoted: number.quoted, Decoded: number.decoded,
		Rejected: number.rejected, Complete: number.complete, NewTarget: number.newTarget,
		HasScalar: number.scalar != nil, HasErrorText: number.errorText != nil,
		Target: legacyNativeCheckpointText{Bytes: len(*number.target)},
	}
	var err error
	state.Scalar, err = legacyNativeCheckpointTypedNumberScalarState(number.scalar)
	if err != nil {
		return nil, err
	}
	state.ErrorText, err = legacyNativeCheckpointTypedNumberScalarState(number.errorText)
	if err != nil {
		return nil, err
	}
	if number.semantic != nil {
		if reflect.TypeOf(number.semantic) != reflect.TypeOf(errLegacyNativeNumberRejected) || number.errorText == nil ||
			number.errorText.text == nil || number.errorText.previous != nil || number.errorText.buffered != 0 ||
			number.semantic.Error() != number.errorText.text.String() {
			return nil, ErrIntegrity
		}
		state.Semantic = 1
	}
	if state.needsSource() {
		state.PartBytes = currentSource.partBytes()
	}
	if err := state.validate(false); err != nil {
		return nil, err
	}
	if state.HasErrorText {
		accepted := number.errorText.text
		if number.errorText.previous != nil {
			accepted = number.errorText.previous
		}
		if accepted == nil || !strings.HasPrefix(accepted.String(), legacyNativeTypedNumberErrorPrefix+`"`) ||
			state.Complete && !strings.HasSuffix(accepted.String(), `"`+legacyNativeTypedNumberErrorSuffix) {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTypedNumberWriter{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(currentSource))
	planned.allocation(int64(len(*number.target)), 1)
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTextWriter{})))
	number.retained(&planned)
	if state.HasScalar || state.HasErrorText {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointScalarWriter{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeScalarSnapshot{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTextWriter{})))
	}
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	currentSource.Read, currentSource.authority = nil, 0
	work := &legacyNativeCheckpointTypedNumberWriter{
		state: state, io: storage, live: currentSource, budget: heapBudget, targetText: string(*number.target),
	}
	targetText := work.targetText
	work.target, err = newLegacyNativeCheckpointTextWriter(ctx, len(targetText), func(current context.Context, offset, length int) ([]byte, error) {
		if err := current.Err(); err != nil {
			return nil, err
		}
		if offset < 0 || offset > len(targetText) || length < 0 || length > LegacyCanonicalEncodingBytes || length > len(targetText)-offset {
			return nil, ErrIntegrity
		}
		return []byte(targetText[offset : offset+length]), current.Err()
	}, storage)
	if err == nil && state.HasScalar {
		work.scalar, err = newLegacyNativeCheckpointScalarWriter(ctx, number.scalar, storage)
	}
	if err == nil && state.HasErrorText {
		work.errorText, err = newLegacyNativeCheckpointScalarWriter(ctx, number.errorText, storage)
	}
	if err == nil {
		err = work.checkBudget(0)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointTypedNumberWriter) captureSource(ctx context.Context) error {
	first, limit := work.state.sourceRange()
	for actions := 0; actions < legacyNativeCheckpointPageItems && work.sourceNext < len(work.live.Parts); actions++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ordinal := work.sourceNext
		if err := legacyNativeCheckpointNumberLivePart(work.live, ordinal); err != nil {
			return err
		}
		if ordinal >= first && ordinal < limit {
			part := work.live.Parts[ordinal]
			if err := work.parts.append(ctx, work.io, legacyNativeCheckpointNumberSourcePart{
				Ordinal: ordinal, Hash: ContentHash(strings.Clone(string(part.Hash))), Bytes: int(part.Bytes),
			}); err != nil {
				return err
			}
		}
		work.sourceNext++
	}
	if work.sourceNext == len(work.live.Parts) {
		if work.state.needsSource() {
			tree, err := work.parts.finish(ctx, work.io)
			if err != nil {
				return err
			}
			work.state.SourceParts = tree
		}
		work.parts.close()
		work.live = LegacyByteSource{}
		work.sourceDone = true
	}
	return ctx.Err()
}

func (work *legacyNativeCheckpointTypedNumberWriter) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if err := work.checkBudget(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	var err error
	switch {
	case !work.sourceDone:
		err = work.captureSource(ctx)
	case work.target != nil:
		var ready bool
		ready, err = work.target.step(ctx)
		if err == nil && ready {
			work.state.Target, err = work.target.take(ctx)
			work.target.close()
			work.target, work.targetText = nil, ""
		}
	case work.scalar != nil:
		var ready bool
		ready, err = work.scalar.step(ctx)
		if err == nil && ready {
			work.state.Scalar, err = work.scalar.take(ctx)
			work.scalar.close()
			work.scalar = nil
		}
	case work.errorText != nil:
		var ready bool
		ready, err = work.errorText.step(ctx)
		if err == nil && ready {
			work.state.ErrorText, err = work.errorText.take(ctx)
			work.errorText.close()
			work.errorText = nil
		}
	default:
		if err = work.state.validate(true); err == nil {
			work.ref, err = legacyNativeCheckpointWriteJSON(ctx, work.io, work.state)
		}
		if err == nil && (!work.ref.valid() || work.ref.Bytes > legacyNativeCheckpointTypedNumberMetadataBytes) {
			err = ErrIntegrity
		}
		work.ready = err == nil
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = work.checkBudget(0)
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointTypedNumberWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.live))
	heap.allocation(int64(len(work.targetText)), 1)
	for _, hash := range []ContentHash{work.ref.Hash, work.state.Target.Tree.Ref.Hash, work.state.Scalar.Text.Tree.Ref.Hash,
		work.state.ErrorText.Text.Tree.Ref.Hash, work.state.SourceParts.Ref.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	for _, scalar := range []*legacyNativeCheckpointScalarWriter{work.scalar, work.errorText} {
		if scalar != nil {
			retained, err := scalar.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
			if scalar.snapshot != nil {
				heap.allocation(int64(scalar.snapshot.capacity), 1)
			}
		}
	}
	retained, err := work.target.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.parts.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func legacyNativeCheckpointTypedNumberAdmit(retained int64, failure error, budget, scratch int64) error {
	if failure != nil {
		return failure
	}
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.allocation(scratch, 1)
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointTypedNumberWriter) checkBudget(scratch int64) error {
	retained, err := work.retainedBytes()
	return legacyNativeCheckpointTypedNumberAdmit(retained, err, work.budget, scratch)
}

func (work *legacyNativeCheckpointTypedNumberWriter) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, work.failure
	}
	if !work.ready || !work.ref.valid() {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, work.failure
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeCheckpointTypedNumberWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointTypedNumberWriter) close() {
	if work != nil {
		work.scalar.close()
		work.errorText.close()
		work.target.close()
		work.parts.close()
		failure := work.failure
		*work = legacyNativeCheckpointTypedNumberWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointTypedNumberRestore struct {
	state        legacyNativeCheckpointTypedNumberState
	io           legacyNativeCheckpointIO
	ref          legacyNativeCheckpointRef
	live         LegacyByteSource
	parts        []ConversationManifestChunk
	sourceParts  *legacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart]
	targetCursor *legacyNativeCheckpointTreeCursor
	scalar       *legacyNativeCheckpointScalarRestore
	number       *legacyNativeTypedNumber
	prior        *json.Number
	priorText    string
	budget       int64
	partCopied   int
	sourceNext   int
	targetCopied int
	checked      int
	checkedMode  legacyJSONValueMode
	loaded       bool
	sourceDone   bool
	targetDone   bool
	textChecked  bool
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeCheckpointTypedNumberRestore(ctx context.Context, ref legacyNativeCheckpointRef, storage legacyNativeCheckpointIO, currentSource LegacyByteSource, prior *json.Number, heapBudget int64) (*legacyNativeCheckpointTypedNumberRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > legacyNativeCheckpointTypedNumberMetadataBytes ||
		storage.Read == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(currentSource, currentSource.Bytes); err != nil {
		return nil, err
	}
	priorText := ""
	if prior != nil {
		priorText = string(*prior)
		if uint64(len(priorText)) > legacyNativeValueCheckpointPayloadLimit() {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTypedNumberRestore{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	planned.include(legacyNativeAnyCheckpointSourceHeap(currentSource))
	planned.allocation(int64(len(currentSource.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	planned.allocation(int64(len(currentSource.Parts)), 128)
	planned.allocation(int64(len(priorText)), 1)
	if prior != nil {
		planned.allocation(1, int64(unsafe.Sizeof(*prior)))
	}
	planned.allocation(int64(16*ref.Bytes), 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	work := &legacyNativeCheckpointTypedNumberRestore{
		io: storage, ref: ref, live: currentSource, prior: prior, priorText: priorText, budget: heapBudget,
		parts: make([]ConversationManifestChunk, len(currentSource.Parts)),
	}
	if err := ctx.Err(); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointTypedNumberRestore) restoreSource(ctx context.Context) error {
	if work.parts != nil {
		for actions := 0; actions < legacyNativeCheckpointPageItems && work.partCopied < len(work.parts); actions++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			ordinal := work.partCopied
			if err := legacyNativeCheckpointNumberLivePart(work.live, ordinal); err != nil {
				return err
			}
			part := work.live.Parts[ordinal]
			part.Hash = ContentHash(strings.Clone(string(part.Hash)))
			work.parts[ordinal] = part
			work.partCopied++
		}
		if work.partCopied == len(work.parts) {
			work.live.Parts, work.parts = work.parts, nil
		}
		return ctx.Err()
	}
	first, limit := work.state.sourceRange()
	if first == limit {
		work.sourceDone = true
		return nil
	}
	if work.sourceParts == nil {
		reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart](work.state.SourceParts, work.io)
		if err != nil {
			return err
		}
		work.sourceParts, work.sourceNext = reader, first
	}
	part, present, err := work.sourceParts.next(ctx)
	if err != nil {
		return err
	}
	if !present {
		if work.sourceNext != limit {
			return ErrIntegrity
		}
		work.sourceParts.close()
		work.sourceParts = nil
		work.sourceDone = true
		return nil
	}
	if work.sourceNext >= limit || part.Ordinal != work.sourceNext || len(part.Hash) != len(contentHashPrefix)+64 ||
		ValidateContentHash(part.Hash) != nil || part.Ordinal < 0 || part.Ordinal >= len(work.live.Parts) ||
		work.live.Parts[part.Ordinal].Hash != part.Hash || work.live.Parts[part.Ordinal].Bytes != int64(part.Bytes) {
		return ErrIntegrity
	}
	work.sourceNext++
	return ctx.Err()
}

func (work *legacyNativeCheckpointTypedNumberRestore) restoreTarget(ctx context.Context) error {
	if work.targetCursor == nil {
		cursor, err := newLegacyNativeCheckpointTreeCursor(work.state.Target.Tree, work.io)
		if err != nil {
			return err
		}
		work.targetCursor = cursor
	}
	ref, count, present, err := work.targetCursor.next(ctx)
	if err != nil {
		return err
	}
	if !present {
		if work.targetCopied != len(work.priorText) {
			return ErrIntegrity
		}
		work.targetCursor.close()
		work.targetCursor = nil
		work.targetDone = true
		return nil
	}
	length := min(LegacyCanonicalEncodingBytes, len(work.priorText)-work.targetCopied)
	if length <= 0 || count != uint64(length) || ref.Bytes != length {
		return ErrIntegrity
	}
	body, err := legacyNativeCheckpointRead(ctx, work.io, ref)
	if err != nil {
		return err
	}
	if string(body) != work.priorText[work.targetCopied:work.targetCopied+length] {
		return ErrIntegrity
	}
	work.targetCopied += length
	return ctx.Err()
}

func (work *legacyNativeCheckpointTypedNumberRestore) startNumber() error {
	retained, err := work.retainedBytes()
	planned := docRangeHeaderHeapEstimate{bytes: retained, err: err}
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeTypedNumber{})))
	if work.state.NewTarget {
		planned.allocation(1, int64(unsafe.Sizeof(json.Number(""))))
	}
	if planned.err != nil {
		return planned.err
	}
	if planned.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	state := work.state
	target := work.prior
	if state.NewTarget {
		target = new(json.Number)
	}
	work.number = &legacyNativeTypedNumber{
		target: target, authority: work.live.authority, sourceBytes: state.SourceBytes,
		from: state.From, end: state.End, field: state.Field, validated: state.Validated, mode: state.Mode,
		diagnostics: state.Diagnostics, quoted: state.Quoted, decoded: state.Decoded, rejected: state.Rejected,
		complete: state.Complete, newTarget: state.NewTarget,
	}
	return nil
}

func (work *legacyNativeCheckpointTypedNumberRestore) restoreScalar(ctx context.Context, saved legacyNativeCheckpointScalar, diagnostic bool) error {
	if work.scalar == nil {
		retained, err := work.retainedBytes()
		if err != nil {
			return err
		}
		if retained >= work.budget {
			return ErrDocumentResourceLimit
		}
		work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, saved, work.io, work.state.SourceBytes, work.budget-retained)
		return err
	}
	ready, err := work.scalar.step(ctx)
	if err != nil || !ready {
		return err
	}
	scalar, err := work.scalar.take(ctx)
	if err != nil {
		return err
	}
	work.scalar.close()
	work.scalar = nil
	if scalar == nil || scalar.previous != nil || scalar.buffered != 0 ||
		scalar.position != saved.State.Position || scalar.end != saved.State.End || scalar.maximum != saved.State.Maximum ||
		scalar.expanded != saved.State.Expanded || (scalar.text != nil) != saved.State.Present ||
		scalar.text != nil && (scalar.text.Len() != saved.State.Bytes || scalar.text.Cap() < saved.State.Capacity) {
		return ErrIntegrity
	}
	if diagnostic {
		if scalar.text == nil || !strings.HasPrefix(scalar.text.String(), legacyNativeTypedNumberErrorPrefix+`"`) ||
			work.state.Complete && !strings.HasSuffix(scalar.text.String(), `"`+legacyNativeTypedNumberErrorSuffix) {
			return ErrIntegrity
		}
		work.number.errorText = scalar
	} else {
		work.number.scalar = scalar
	}
	return nil
}

func (work *legacyNativeCheckpointTypedNumberRestore) checkText(ctx context.Context) error {
	if work.state.Rejected {
		work.textChecked = true
		return nil
	}
	scalar := work.number.scalar
	if scalar == nil || work.state.Validated > 0 && scalar.text == nil {
		return ErrIntegrity
	}
	if scalar.text != nil {
		text := scalar.text.String()
		limit := min(work.state.Validated, work.checked+LegacyCanonicalEncodingBytes)
		for work.checked < limit {
			mode, valid := legacyJSONNumberNext(work.checkedMode, text[work.checked])
			if !valid {
				return ErrIntegrity
			}
			work.checkedMode = mode
			work.checked++
		}
	}
	if work.checked == work.state.Validated {
		if work.checkedMode != work.state.Mode {
			return ErrIntegrity
		}
		work.textChecked = true
	}
	return ctx.Err()
}

func (work *legacyNativeCheckpointTypedNumberRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if err := work.checkBudget(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	var err error
	switch {
	case !work.loaded:
		if err = work.checkBudget(int64(16 * work.ref.Bytes)); err == nil {
			err = legacyNativeCheckpointReadJSON(ctx, work.io, work.ref, &work.state)
		}
		if err == nil {
			err = work.state.validate(true)
		}
		if err == nil && (work.state.SourceBytes != work.live.Bytes ||
			work.state.needsSource() && work.state.PartBytes != work.live.partBytes() ||
			work.state.NewTarget != (work.prior == nil) || work.state.Target.Bytes != len(work.priorText)) {
			err = ErrIntegrity
		}
		work.loaded = err == nil
	case !work.sourceDone:
		err = work.restoreSource(ctx)
	case !work.targetDone:
		err = work.restoreTarget(ctx)
	case work.number == nil:
		err = work.startNumber()
	case work.state.HasScalar && work.number.scalar == nil:
		err = work.restoreScalar(ctx, work.state.Scalar, false)
	case work.state.HasErrorText && work.number.errorText == nil:
		err = work.restoreScalar(ctx, work.state.ErrorText, true)
	case !work.textChecked:
		err = work.checkText(ctx)
	default:
		if work.prior != nil && string(*work.prior) != work.priorText {
			err = ErrIntegrity
		} else if work.state.Semantic == 1 {
			if err = work.checkBudget(int64(unsafe.Sizeof(string("")))); err == nil {
				work.number.semantic = errors.New(work.number.errorText.text.String())
			}
		}
		work.ready = err == nil
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = work.checkBudget(0)
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointTypedNumberRestore) source() LegacyByteSource {
	if work == nil || work.closed || work.failure != nil || !work.ready || !work.sourceDone || work.number == nil {
		return LegacyByteSource{}
	}
	live := work.live
	first, limit := work.state.sourceRange()
	if first == limit {
		return live
	}
	store := work.io
	restored := live
	restored.checkpointReaderHeap.include(legacyNativeCheckpointNumberReaderHeap())
	restored.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		partBytes := live.partBytes()
		if offset >= 0 && offset < live.Bytes && offset%partBytes == 0 {
			ordinal := offset / partBytes
			if ordinal >= first && ordinal < limit {
				part := live.Parts[ordinal]
				if length != int(part.Bytes) {
					return nil, ErrIntegrity
				}
				dependency := live
				dependency.Read = func(current context.Context, readOffset, readLength int) ([]byte, error) {
					if readOffset != offset || readLength != length {
						return nil, ErrIntegrity
					}
					return store.Read(current, part.Hash, readLength)
				}
				return readLegacyByteSourcePart(ctx, dependency, ordinal)
			}
		}
		return live.Read(ctx, offset, length)
	}
	return restored
}

func (work *legacyNativeCheckpointTypedNumberRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.live))
	if first, limit := work.state.sourceRange(); first != limit {
		heap.include(legacyNativeCheckpointNumberReaderHeap())
	}
	if work.parts != nil {
		heap.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
		heap.allocation(int64(work.partCopied), 128)
	}
	heap.allocation(int64(len(work.priorText)), 1)
	if work.prior != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.prior)))
	}
	for _, hash := range []ContentHash{work.ref.Hash, work.state.Target.Tree.Ref.Hash, work.state.Scalar.Text.Tree.Ref.Hash,
		work.state.ErrorText.Text.Tree.Ref.Hash, work.state.SourceParts.Ref.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	if work.number != nil {
		work.number.retained(&heap)
	}
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.sourceParts != nil {
		retained, err := work.sourceParts.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.targetCursor != nil {
		retained, err := work.targetCursor.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointTypedNumberRestore) checkBudget(scratch int64) error {
	retained, err := work.retainedBytes()
	return legacyNativeCheckpointTypedNumberAdmit(retained, err, work.budget, scratch)
}

func (work *legacyNativeCheckpointTypedNumberRestore) take(ctx context.Context) (*legacyNativeTypedNumber, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if work.closed {
		return nil, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, work.failure
	}
	if !work.ready || work.number == nil || work.prior != nil && string(*work.prior) != work.priorText {
		work.fail(ErrIntegrity)
		return nil, work.failure
	}
	number := work.number
	work.close()
	return number, nil
}

func (work *legacyNativeCheckpointTypedNumberRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointTypedNumberRestore) close() {
	if work != nil {
		work.scalar.close()
		if work.sourceParts != nil {
			work.sourceParts.close()
		}
		work.targetCursor.close()
		failure := work.failure
		*work = legacyNativeCheckpointTypedNumberRestore{closed: true, failure: failure}
	}
}
