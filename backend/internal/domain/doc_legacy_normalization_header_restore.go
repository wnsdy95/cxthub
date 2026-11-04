package domain

import (
	"bytes"
	"context"
	"reflect"
	"unsafe"
)

type legacyNormalizationHeaderCheckpointRestore struct {
	normalizer *LegacyNormalizationDecoder
	header     *legacyNormalizationHeader
	state      legacyNormalizationHeaderCheckpointControl
	ref        legacyNativeCheckpointRef
	binding    ContentHash
	store      legacyNativeCheckpointIO
	native     *legacyNativeObjectCheckpointRestore
	chunks     *legacyNativeCheckpointListReader[legacyNormalizationHeaderCheckpointChunk]
	cursor     *legacyNativeCheckpointTreeCursor
	budget     int64
	chunkBytes int
	copied     int
	phase      uint8
	granted    bool
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNormalizationHeaderCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, normalizer *LegacyNormalizationDecoder, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (_ *legacyNormalizationHeaderCheckpointRestore, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer == nil || normalizer.mutation == nil || !ref.valid() || store.Read == nil || ValidateContentHash(binding) != nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationHeaderCheckpointRestore{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationHeader{})))
	planned.allocation(4*LegacyCanonicalEncodingBytes, 1)
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	if !normalizer.mutation.activity.CompareAndSwap(0, -2) {
		return nil, ErrLegacyNormalizationBusy
	}
	defer func() {
		if failure != nil {
			normalizer.mutation.activity.CompareAndSwap(-2, 0)
		}
	}()
	if normalizer.mutation.closed.Load() || normalizer.mutation.stepping.Load() || normalizer.phase != legacyNormalizationFinalizing || normalizer.finalizing != nil {
		return nil, ErrConflict
	}
	retained, err := normalizer.RetainedBytes()
	planned.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	return &legacyNormalizationHeaderCheckpointRestore{normalizer: normalizer, ref: ref, binding: binding, store: store, budget: budget, granted: true}, nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) valid() error {
	if work.closed || work.normalizer == nil || work.normalizer.mutation.closed.Load() {
		return ErrConflict
	}
	if !work.granted || work.normalizer.mutation.activity.Load() != -2 || work.normalizer.mutation.stepping.Load() {
		return ErrLegacyNormalizationBusy
	}
	return nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) restoreBuffer(ctx context.Context, state legacyNativeCheckpointText, output bool) (bool, error) {
	if state.Bytes == 0 {
		return true, nil
	}
	if work.cursor == nil {
		var planned docRangeHeaderHeapEstimate
		planned.allocation(int64(state.Bytes), 1)
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeCursor{})))
		planned.allocation(int64(len(state.Tree.Ref.Hash)), 1)
		if err := work.admit(planned.bytes); err != nil {
			return false, err
		}
		buffer := make([]byte, state.Bytes)
		if output {
			work.header.output = buffer
		} else {
			work.header.header = buffer
		}
		cursor, err := newLegacyNativeCheckpointTreeCursor(state.Tree, work.store)
		if err != nil {
			return false, err
		}
		work.cursor, work.copied = cursor, 0
		return false, nil
	}
	ref, count, present, err := work.cursor.next(ctx)
	if err != nil {
		return false, err
	}
	if !present {
		if work.copied != state.Bytes {
			return false, ErrIntegrity
		}
		work.cursor.close()
		work.cursor = nil
		return true, nil
	}
	if count != uint64(ref.Bytes) || ref.Bytes > LegacyCanonicalEncodingBytes || ref.Bytes > state.Bytes-work.copied {
		return false, ErrIntegrity
	}
	body, err := legacyNativeCheckpointRead(ctx, work.store, ref)
	if err != nil {
		return false, err
	}
	buffer := work.header.header
	if output {
		buffer = work.header.output
	}
	copy(buffer[work.copied:work.copied+len(body)], body)
	work.copied += len(body)
	return false, nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.binding, work.normalizer.source.Bytes, true); err != nil {
			return err
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationHeader{})))
		if work.state.Encoding != nil {
			planned.allocation(1, int64(unsafe.Sizeof(LegacyEnvelopeEncoding{})))
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		work.header = &legacyNormalizationHeader{length: work.state.Length}
		if work.state.Encoding != nil {
			work.header.encoding = &LegacyEnvelopeEncoding{}
		}
		work.normalizer.finalizing = work.header
		work.phase++
	case 1:
		if work.state.Encoding == nil || !work.state.Encoding.HasNative {
			work.phase++
			return nil
		}
		if work.native == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			shared, err := work.normalizer.RetainedBytes()
			if err != nil {
				return err
			}
			owner := legacyNativeObjectCheckpointOwner{decoder: work.normalizer, header: work.header}
			work.native, err = newLegacyNativeObjectCheckpointRestoreForOwner(ctx, work.state.Envelope, legacyNativeCheckpointEnvelopeModel, work.store, work.normalizer.source, work.binding, remaining+shared, owner)
			return err
		}
		ready, err := work.native.step(ctx)
		if err != nil || !ready {
			return err
		}
		result, err := work.native.take(ctx)
		work.native = nil
		if err != nil {
			return err
		}
		if result.Scanner != (legacyJSONValueScanner{}) || result.Object.destination.Interface() != &work.header.encoding.envelope || result.Object.phase != legacyNativeObjectDone || result.Object.parentDepth != 2 || result.Object.trackPresence {
			return ErrIntegrity
		}
		work.header.native = result.Object
		for _, field := range result.Object.heaps {
			work.header.encoding.envelopeHeap.include(field)
		}
		work.normalizer.source = result.Source
		work.phase++
	case 2:
		if work.state.Encoding != nil {
			if err := work.restoreEncoding(ctx); err != nil {
				return err
			}
		}
		work.phase++
	case 3:
		if work.chunks == nil {
			var err error
			work.chunks, err = newLegacyNativeCheckpointListReader[legacyNormalizationHeaderCheckpointChunk](work.state.Chunks, work.store)
			return err
		}
		row, present, err := work.chunks.next(ctx)
		if err != nil {
			return err
		}
		if !present {
			want := work.state.Length - work.state.Position
			if work.state.Copying {
				want = 0
			}
			if work.chunkBytes != want {
				return ErrIntegrity
			}
			work.chunks.close()
			work.chunks = nil
			work.phase++
			return nil
		}
		if err := row.validate(); err != nil {
			return err
		}
		if row.Body.Bytes > work.state.Length-work.chunkBytes {
			return ErrIntegrity
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationHeaderChunk{})))
		planned.allocation(int64(row.Capacity), 1)
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		body, err := legacyNativeCheckpointRead(ctx, work.store, row.Body)
		if err != nil {
			return err
		}
		chunk := &legacyNormalizationHeaderChunk{body: make([]byte, len(body), row.Capacity)}
		copy(chunk.body, body)
		if work.header.last != nil {
			if len(work.header.last.body) != cap(work.header.last.body) {
				return ErrIntegrity
			}
			work.header.last.next = chunk
		} else {
			work.header.first = chunk
		}
		work.header.last = chunk
		work.header.heap.include(planned)
		work.chunkBytes += len(body)
	case 4, 5:
		state := work.state.Header
		if work.phase == 5 {
			state = work.state.Output
		}
		ready, err := work.restoreBuffer(ctx, state, work.phase == 5)
		if err != nil || !ready {
			return err
		}
		work.phase++
	case 6:
		work.header.position, work.header.copying = work.state.Position, work.state.Copying
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) restoreEncoding(ctx context.Context) error {
	encoding := work.header.encoding
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(LegacyEnvelopeEncoding{})))
	planned.allocation(int64(len(legacyEnvelopeNativeFields.fields)), int64(unsafe.Sizeof(legacyEnvelopeEncodingField{}))+128)
	planned.allocation(1, int64(unsafe.Sizeof(LegacyCanonicalEncoding{})))
	planned.allocation(4*LegacyCanonicalEncodingBytes, 1)
	if err := work.admit(planned.bytes); err != nil {
		return err
	}
	rebuilt, err := newLegacyEnvelopeEncoding(ctx, encoding.envelope, encoding.envelopeHeap)
	if err != nil {
		return err
	}
	state := work.state.Encoding
	if ValidateCIRVersion(CIRDocument{Envelope: rebuilt.envelope}) != nil || work.normalizer.requiresV2 && rebuilt.envelope.CIRVersion != CIRVersionV2 {
		return ErrIntegrity
	}
	if state.Field > len(rebuilt.fields) || state.Phase == legacyEnvelopeEncodingStart && (state.Field != 0 || state.Model != 0 || state.Scalar != nil || state.HasPending) ||
		state.Phase != legacyEnvelopeEncodingStart && state.Phase != legacyEnvelopeEncodingFieldNext && state.Field >= len(rebuilt.fields) ||
		state.Phase == legacyEnvelopeEncodingModelNext && (reflect.ValueOf(&rebuilt.envelope).Elem().Field(rebuilt.fields[state.Field].index).Kind() != reflect.Slice || state.Model > len(rebuilt.envelope.SourceModels)) ||
		state.Phase == legacyEnvelopeEncodingModelValue && (reflect.ValueOf(&rebuilt.envelope).Elem().Field(rebuilt.fields[state.Field].index).Kind() != reflect.Slice || state.Model >= len(rebuilt.envelope.SourceModels)) {
		return ErrIntegrity
	}
	rebuilt.phase, rebuilt.field, rebuilt.model = state.Phase, state.Field, state.Model
	scalar, err := restoreLegacyEnvelopeScalar(ctx, rebuilt, state.Scalar)
	if err != nil {
		return err
	}
	rebuilt.scalar = scalar
	if scalar != nil {
		if text, ok := scalar.root.(string); ok {
			rebuilt.scalarBorrowed.allocation(int64(len(text)), 1)
		}
	}
	if state.HasPending {
		body, err := legacyNativeCheckpointRead(ctx, work.store, state.Pending)
		if err != nil {
			return err
		}
		rebuilt.pending = bytes.Clone(body)
	}
	*encoding = *rebuilt
	return nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(13*(len(contentHashPrefix)+64)), 1)
	if work.state.Encoding != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.state.Encoding)))
		if work.state.Encoding.Scalar != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*work.state.Encoding.Scalar)))
		}
	}
	for _, task := range []interface{ retainedBytes() (int64, error) }{work.chunks, work.cursor} {
		retained, err := task.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.native != nil {
		retained, err := work.native.retainedBytes()
		shared, sharedErr := work.native.owner.retainedBytes()
		if err != nil || sharedErr != nil || shared > retained {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - shared})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationHeaderCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	shared, err := work.normalizer.RetainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget || shared > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - shared, nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.valid(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.valid(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) take(ctx context.Context) (*legacyNormalizationHeader, error) {
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
	if !work.ready || work.closed {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	if err := work.valid(); err != nil {
		work.fail(err)
		return nil, err
	}
	header := work.header
	work.close()
	return header, nil
}

func (work *legacyNormalizationHeaderCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNormalizationHeaderCheckpointRestore) close() {
	if work != nil && !work.closed {
		work.native.close()
		work.chunks.close()
		work.cursor.close()
		if work.granted {
			work.normalizer.mutation.activity.CompareAndSwap(-2, 0)
		}
		failure := work.failure
		*work = legacyNormalizationHeaderCheckpointRestore{closed: true, failure: failure}
	}
}
