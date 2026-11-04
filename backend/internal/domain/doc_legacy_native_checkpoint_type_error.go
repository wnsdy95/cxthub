package domain

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const legacyNativeCheckpointTypeErrorNameBytes = 4096
const legacyNativeCheckpointTypeErrorMetadataBytes = 1024 + 12*legacyNativeCheckpointTypeErrorNameBytes
const legacyNativeCheckpointTypeErrorTextScratch = 4 * LegacyCanonicalEncodingBytes

type legacyNativeCheckpointTypeErrorState struct {
	Version     uint8                      `json:"version"`
	TypeID      uint8                      `json:"type_id"`
	SourceBytes int                        `json:"source_bytes"`
	Offset      int64                      `json:"offset"`
	Struct      string                     `json:"struct"`
	Field       string                     `json:"field"`
	Value       legacyNativeCheckpointText `json:"value"`
}

func (state legacyNativeCheckpointTypeErrorState) validateHeader(sourceBytes int) error {
	if state.Version != legacyNativeCheckpointVersion || sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes ||
		state.SourceBytes != sourceBytes || state.Offset < 0 || state.Offset > 2*int64(sourceBytes)+LegacyCanonicalEncodingBytes ||
		len(state.Struct) > legacyNativeCheckpointTypeErrorNameBytes || len(state.Field) > legacyNativeCheckpointTypeErrorNameBytes ||
		!utf8.ValidString(state.Struct) || !utf8.ValidString(state.Field) ||
		state.Value.Bytes < 0 || uint64(state.Value.Bytes) > legacyNativeValueCheckpointPayloadLimit() {
		return ErrIntegrity
	}
	_, err := legacyNativeCheckpointErrorType(state.TypeID)
	return err
}

func (state legacyNativeCheckpointTypeErrorState) validate(sourceBytes int) error {
	if err := state.validateHeader(sourceBytes); err != nil {
		return err
	}
	if !state.Value.Tree.valid() || state.Value.Tree.Count != uint64(state.Value.Bytes) ||
		(state.Value.Bytes == 0) != (state.Value.Tree == (legacyNativeCheckpointTree{})) {
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeCheckpointTypeErrorState) retained(heap *docRangeHeaderHeapEstimate) {
	heap.allocation(int64(len(state.Struct)), 1)
	heap.allocation(int64(len(state.Field)), 1)
	heap.allocation(int64(len(state.Value.Tree.Ref.Hash)), 1)
}

func legacyNativeCheckpointTypeErrorBudget(retained, scratch, budget int64) error {
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

type legacyNativeCheckpointTypeErrorWriter struct {
	state   legacyNativeCheckpointTypeErrorState
	store   legacyNativeCheckpointIO
	text    *legacyNativeCheckpointTextWriter
	value   string
	ref     legacyNativeCheckpointRef
	budget  int64
	ready   bool
	closed  bool
	failure error
}

func newLegacyNativeCheckpointTypeErrorWriter(ctx context.Context, semantic *json.UnmarshalTypeError, store legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointTypeErrorWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if semantic == nil || store.Put == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	typeID, err := legacyNativeCheckpointErrorTypeID(semantic.Type)
	if err != nil {
		return nil, err
	}
	state := legacyNativeCheckpointTypeErrorState{
		Version: legacyNativeCheckpointVersion, TypeID: typeID, SourceBytes: sourceBytes,
		Offset: semantic.Offset, Struct: semantic.Struct, Field: semantic.Field,
		Value: legacyNativeCheckpointText{Bytes: len(semantic.Value)},
	}
	if err := state.validateHeader(sourceBytes); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTypeErrorWriter{})))
	state.retained(&planned)
	planned.allocation(int64(len(semantic.Value)), 1)
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTextWriter{})))
	planned.allocation(1, 64)
	if planned.err != nil {
		return nil, planned.err
	}
	if err := legacyNativeCheckpointTypeErrorBudget(planned.bytes, legacyNativeCheckpointTypeErrorTextScratch, heapBudget); err != nil {
		return nil, err
	}
	state.Struct, state.Field = strings.Clone(state.Struct), strings.Clone(state.Field)
	work := &legacyNativeCheckpointTypeErrorWriter{state: state, store: store, value: semantic.Value, budget: heapBudget}
	work.text, err = newLegacyNativeCheckpointTextWriter(ctx, len(work.value), work.read, store)
	if err != nil {
		work.close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointTypeErrorWriter) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil || work.closed || offset < 0 || offset > len(work.value) || length <= 0 ||
		length > LegacyCanonicalEncodingBytes || length > len(work.value)-offset {
		return nil, ErrIntegrity
	}
	body := []byte(work.value[offset : offset+length])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func (work *legacyNativeCheckpointTypeErrorWriter) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if work.ready {
		return true, nil
	}
	if work.text != nil {
		if err := work.checkBudget(legacyNativeCheckpointTypeErrorTextScratch); err != nil {
			return work.fail(err)
		}
		ready, err := work.text.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.state.Value, err = work.text.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.text.close()
			work.text, work.value = nil, ""
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if err := work.state.validate(work.state.SourceBytes); err != nil {
		return work.fail(err)
	}
	maximum := 1024 + 6*(len(work.state.Struct)+len(work.state.Field))
	if err := work.checkBudget(8*int64(maximum) + 4096); err != nil {
		return work.fail(err)
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
	if err != nil {
		return work.fail(err)
	}
	if !ref.valid() || ref.Bytes > maximum || ref.Bytes > legacyNativeCheckpointTypeErrorMetadataBytes {
		return work.fail(ErrIntegrity)
	}
	work.ref = ref
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return true, nil
}

func (work *legacyNativeCheckpointTypeErrorWriter) checkBudget(scratch int64) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointTypeErrorBudget(retained, scratch, work.budget)
}

func (work *legacyNativeCheckpointTypeErrorWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	work.state.retained(&heap)
	heap.allocation(int64(len(work.value)), 1)
	heap.allocation(int64(len(work.ref.Hash)), 1)
	if work.text != nil {
		retained, err := work.text.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		heap.allocation(1, 64)
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointTypeErrorWriter) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if !work.ready || !work.ref.valid() {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeCheckpointTypeErrorWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointTypeErrorWriter) close() {
	if work != nil {
		work.text.close()
		failure := work.failure
		*work = legacyNativeCheckpointTypeErrorWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointTypeErrorRestore struct {
	state       legacyNativeCheckpointTypeErrorState
	store       legacyNativeCheckpointIO
	ref         legacyNativeCheckpointRef
	scalar      *legacyNativeCheckpointScalarRestore
	value       *legacyNativeScalar
	result      *json.UnmarshalTypeError
	sourceBytes int
	budget      int64
	loaded      bool
	ready       bool
	closed      bool
	failure     error
}

func newLegacyNativeCheckpointTypeErrorRestore(ctx context.Context, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointTypeErrorRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > legacyNativeCheckpointTypeErrorMetadataBytes || store.Read == nil ||
		sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTypeErrorRestore{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if err := legacyNativeCheckpointTypeErrorBudget(planned.bytes, 8*int64(ref.Bytes)+4096, heapBudget); err != nil {
		return nil, err
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &legacyNativeCheckpointTypeErrorRestore{store: store, ref: ref, sourceBytes: sourceBytes, budget: heapBudget}, nil
}

func (work *legacyNativeCheckpointTypeErrorRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if work.ready {
		return true, nil
	}
	if !work.loaded {
		if err := work.checkBudget(8*int64(work.ref.Bytes) + 4096); err != nil {
			return work.fail(err)
		}
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return work.fail(err)
		}
		if err := work.state.validate(work.sourceBytes); err != nil {
			return work.fail(err)
		}
		work.loaded = true
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.scalar == nil && work.value == nil {
		retained, err := work.retainedBytes()
		if err != nil {
			return work.fail(err)
		}
		var scratch docRangeHeaderHeapEstimate
		scratch.allocation(legacyNativeCheckpointTypeErrorTextScratch, 1)
		if scratch.err != nil {
			return work.fail(scratch.err)
		}
		if retained >= work.budget || scratch.bytes >= work.budget-retained {
			return work.fail(ErrDocumentResourceLimit)
		}
		saved := legacyNativeCheckpointScalar{
			State: legacyNativeScalarCheckpoint{
				Version: 1, Maximum: work.state.Value.Bytes, Bytes: work.state.Value.Bytes,
				Capacity: work.state.Value.Bytes, Present: true,
			},
			Text: work.state.Value,
		}
		work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, saved, work.store, work.sourceBytes, work.budget-retained-scratch.bytes)
		if err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(legacyNativeCheckpointTypeErrorTextScratch); err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.scalar != nil {
		if err := work.checkBudget(legacyNativeCheckpointTypeErrorTextScratch); err != nil {
			return work.fail(err)
		}
		ready, err := work.scalar.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.value, err = work.scalar.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.scalar.close()
			work.scalar = nil
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.value.text == nil || work.value.previous != nil || work.value.position != 0 || work.value.end != 0 ||
		work.value.buffered != 0 || work.value.expanded || work.value.maximum != work.state.Value.Bytes ||
		work.value.text.Len() != work.state.Value.Bytes || work.value.text.Cap() < work.state.Value.Bytes {
		return work.fail(ErrIntegrity)
	}
	model, err := legacyNativeCheckpointErrorType(work.state.TypeID)
	if err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(int64(unsafe.Sizeof(json.UnmarshalTypeError{}))); err != nil {
		return work.fail(err)
	}
	work.result = &json.UnmarshalTypeError{
		Value: work.value.text.String(), Type: model, Offset: work.state.Offset,
		Struct: work.state.Struct, Field: work.state.Field,
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return true, nil
}

func (work *legacyNativeCheckpointTypeErrorRestore) checkBudget(scratch int64) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointTypeErrorBudget(retained, scratch, work.budget)
}

func (work *legacyNativeCheckpointTypeErrorRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	work.state.retained(&heap)
	heap.allocation(int64(len(work.ref.Hash)), 1)
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.value != nil {
		work.value.retained(&heap)
	}
	if work.result != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.result)))
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointTypeErrorRestore) take(ctx context.Context) (*json.UnmarshalTypeError, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, err
	}
	if work.closed {
		return nil, ErrIntegrity
	}
	if !work.ready || work.result == nil {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointTypeErrorRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointTypeErrorRestore) close() {
	if work != nil {
		work.scalar.close()
		failure := work.failure
		*work = legacyNativeCheckpointTypeErrorRestore{closed: true, failure: failure}
	}
}
