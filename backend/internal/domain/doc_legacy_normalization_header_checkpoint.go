package domain

import (
	"context"
	"reflect"
	"unsafe"
)

type legacyNormalizationHeaderCheckpoint struct {
	normalizer *LegacyNormalizationDecoder
	header     *legacyNormalizationHeader
	capture    *legacyNormalizationCapture
	state      legacyNormalizationHeaderCheckpointControl
	store      legacyNativeCheckpointIO
	native     *legacyNativeObjectCheckpoint
	text       *legacyNativeCheckpointTextWriter
	chunks     legacyNativeCheckpointListWriter[legacyNormalizationHeaderCheckpointChunk]
	current    *legacyNormalizationHeaderChunk
	idleFrame  legacyJSONValueScanner
	root       legacyNativeCheckpointRef
	sourceHeap int64
	budget     int64
	written    int
	phase      uint8
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNormalizationHeaderCheckpoint(ctx context.Context, normalizer *LegacyNormalizationDecoder, binding ContentHash, store legacyNativeCheckpointIO, available int64) (_ *legacyNormalizationHeaderCheckpoint, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer == nil || store.Put == nil || available <= 0 {
		return nil, ErrIntegrity
	}
	capture, err := normalizer.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	defer func() {
		if failure != nil {
			capture.release()
		}
	}()
	if normalizer.phase != legacyNormalizationFinalizing || normalizer.finalizing == nil {
		return nil, ErrConflict
	}
	header := normalizer.finalizing
	retained, err := header.retained()
	if err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationHeaderCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.allocation(4*LegacyCanonicalEncodingBytes, 1)
	if planned.err != nil || planned.bytes > available {
		return nil, ErrDocumentResourceLimit
	}
	schema, err := legacyNativeCheckpointModelSchema(&legacyEnvelopeNativeFields)
	if err != nil {
		return nil, err
	}
	state := legacyNormalizationHeaderCheckpointControl{Kind: "legacy-normalization-header", Version: 1, Binding: binding, Schema: schema, SourceBytes: normalizer.source.Bytes,
		Length: header.length, Position: header.position, Copying: header.copying,
		Header: legacyNativeCheckpointText{Bytes: len(header.header)}, Output: legacyNativeCheckpointText{Bytes: len(header.output)}}
	if header.encoding != nil {
		encoding := header.encoding
		if !encoding.initialized || encoding.complete || encoding.err != nil || header.native == nil && (!reflect.ValueOf(encoding.envelope).IsZero() || encoding.envelopeHeap != (docRangeHeaderHeapEstimate{})) ||
			header.native != nil && (header.native.destination.Interface() != &encoding.envelope || header.native.mutation != normalizer.mutation || header.native.phase != legacyNativeObjectDone || header.native.metadata != &legacyEnvelopeNativeFields) ||
			encoding.pending != nil && (len(encoding.pending) == 0 || len(encoding.pending) > LegacyCanonicalEncodingBytes || cap(encoding.pending) > LegacyCanonicalEncodingBytes) {
			return nil, ErrIntegrity
		}
		scalar, err := legacyEnvelopeScalarControlFor(encoding)
		if err != nil {
			return nil, err
		}
		state.Encoding = &legacyEnvelopeEncodingCheckpointControl{Phase: encoding.phase, Field: encoding.field, Model: encoding.model,
			HasNative: header.native != nil, HasPending: encoding.pending != nil, Scalar: scalar}
	} else if header.native != nil || len(header.header) != header.length || cap(header.header) != header.length || header.copying && (len(header.output) != header.length || cap(header.output) != header.length) {
		return nil, ErrIntegrity
	}
	if err := state.validate(binding, normalizer.source.Bytes, false); err != nil {
		return nil, err
	}
	budget := docRangeHeaderHeapEstimate{bytes: available}
	budget.include(docRangeHeaderHeapEstimate{bytes: retained})
	if budget.err != nil {
		return nil, budget.err
	}
	return &legacyNormalizationHeaderCheckpoint{normalizer: normalizer, header: header, capture: capture, state: state, store: store,
		current: header.first, sourceHeap: retained, budget: budget.bytes}, nil
}

func (work *legacyNormalizationHeaderCheckpoint) valid() error {
	if work.closed || !work.capture.valid(work.normalizer.mutation) || work.normalizer.finalizing != work.header || work.normalizer.phase != legacyNormalizationFinalizing {
		return ErrConflict
	}
	return nil
}

func (work *legacyNormalizationHeaderCheckpoint) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if work.state.Encoding == nil || !work.state.Encoding.HasNative {
			work.phase++
			return nil
		}
		if work.native == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			retained, err := work.header.native.retained()
			if err != nil {
				return err
			}
			work.native, err = newLegacyNativeObjectCheckpoint(ctx, work.header.native, &work.idleFrame, work.state.Binding, work.normalizer.source, work.store, remaining+retained)
			return err
		}
		ready, err := work.native.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Envelope, err = work.native.take(ctx)
		work.native = nil
		if err != nil {
			return err
		}
		work.phase++
	case 1:
		if work.current == nil {
			want := work.state.Length
			if work.state.Encoding == nil {
				want -= work.state.Position
			}
			if work.state.Copying {
				want = 0
			}
			if work.written != want || (work.header.first == nil) != (work.header.last == nil) {
				return ErrIntegrity
			}
			work.phase++
			return nil
		}
		chunk := work.current
		if len(chunk.body) == 0 || len(chunk.body) > work.state.Length-work.written || chunk.next == nil && chunk != work.header.last {
			return ErrIntegrity
		}
		ref, err := legacyNativeCheckpointWrite(ctx, work.store, chunk.body)
		if err != nil {
			return err
		}
		row := legacyNormalizationHeaderCheckpointChunk{Capacity: cap(chunk.body), Body: ref}
		if err := row.validate(); err != nil {
			return err
		}
		if err := work.chunks.append(ctx, work.store, row); err != nil {
			return err
		}
		work.current, work.written = chunk.next, work.written+len(chunk.body)
	case 2:
		var err error
		work.state.Chunks, err = work.chunks.finish(ctx, work.store)
		if err != nil {
			return err
		}
		work.phase++
	case 3, 4:
		body := work.header.header
		if work.phase == 4 {
			body = work.header.output
		}
		if body == nil {
			work.phase++
			return nil
		}
		if work.text == nil {
			var err error
			work.text, err = newLegacyNativeCheckpointTextWriter(ctx, len(body), func(ctx context.Context, offset, length int) ([]byte, error) {
				if err := work.valid(); err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || length > LegacyCanonicalEncodingBytes || offset > len(body) || length > len(body)-offset {
					return nil, ErrIntegrity
				}
				return body[offset : offset+length], nil
			}, work.store)
			return err
		}
		ready, err := work.text.step(ctx)
		if err != nil || !ready {
			return err
		}
		text, err := work.text.take(ctx)
		work.text = nil
		if err != nil {
			return err
		}
		if work.phase == 3 {
			work.state.Header = text
		} else {
			work.state.Output = text
		}
		work.phase++
	case 5:
		if work.state.Encoding != nil && work.state.Encoding.HasPending {
			ref, err := legacyNativeCheckpointWrite(ctx, work.store, work.header.encoding.pending)
			if err != nil {
				return err
			}
			work.state.Encoding.Pending = ref
		}
		work.phase++
	case 6:
		if err := work.state.validate(work.state.Binding, work.normalizer.source.Bytes, true); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
		if err != nil {
			return err
		}
		work.root, work.ready = ref, true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNormalizationHeaderCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	heap.allocation(int64(13*(len(contentHashPrefix)+64)), 1)
	if work.state.Encoding != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.state.Encoding)))
		if work.state.Encoding.Scalar != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*work.state.Encoding.Scalar)))
		}
	}
	for _, task := range []interface{ retainedBytes() (int64, error) }{work.native, work.text, &work.chunks} {
		retained, err := task.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationHeaderCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget || work.sourceHeap > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - work.sourceHeap, nil
}

func (work *legacyNormalizationHeaderCheckpoint) step(ctx context.Context) (bool, error) {
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
	if remaining, err := work.remaining(); err != nil || remaining < 4*LegacyCanonicalEncodingBytes {
		if err == nil {
			err = ErrDocumentResourceLimit
		}
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
	if _, err := work.remaining(); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNormalizationHeaderCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	if !work.ready || work.closed {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.valid(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	root := work.root
	work.close()
	return root, nil
}

func (work *legacyNormalizationHeaderCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNormalizationHeaderCheckpoint) close() {
	if work != nil && !work.closed {
		work.native.close()
		work.text.close()
		work.chunks.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNormalizationHeaderCheckpoint{closed: true, failure: failure}
	}
}
