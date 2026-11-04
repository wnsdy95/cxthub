package domain

import (
	"context"
	"strings"
	"unsafe"
)

type legacyNormalizationDecoderCheckpointRestore struct {
	control    legacyNormalizationDecoderControl
	normalizer *LegacyNormalizationDecoder
	event      *legacyNormalizationNativeEvent
	child      *legacyNativeObjectCheckpointRestore
	header     *legacyNormalizationHeaderCheckpointRestore
	ref        legacyNativeCheckpointRef
	binding    ContentHash
	store      legacyNativeCheckpointIO
	budget     int64
	phase      uint8
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNormalizationDecoderCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, source LegacyByteSource, binding ContentHash, budget int64) (*legacyNormalizationDecoderCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	partBytes := source.partBytes()
	if source.Bytes <= 0 || source.Bytes > MaxFinalizedDocBytes || partBytes == 0 || source.Read == nil || len(source.Parts) != 1+(source.Bytes-1)/partBytes {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationDecoderCheckpointRestore{})))
	planned.allocation(1, int64(unsafe.Sizeof(LegacyNormalizationDecoder{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(ref.Hash)+len(binding)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
	if err != nil {
		return nil, err
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	return &legacyNormalizationDecoderCheckpointRestore{normalizer: normalizer, ref: ref, binding: ContentHash(strings.Clone(string(binding))), store: store, budget: budget}, nil
}

func (work *legacyNormalizationDecoderCheckpointRestore) advance(ctx context.Context) error {
	if work.phase == 0 {
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.control); err != nil {
			return err
		}
		if err := work.control.validate(ctx, work.binding, work.normalizer.source, true); err != nil {
			return err
		}
		frame, err := restoreLegacyJSONValueScanner(ctx, work.control.Frame)
		if err != nil {
			return err
		}
		normalizer, control := work.normalizer, work.control
		normalizer.position, normalizer.phase, normalizer.frame = control.Position, control.Phase, frame
		normalizer.valueStart, normalizer.field, normalizer.key, normalizer.keyBytes = control.ValueStart, control.Field, control.Key, control.KeyBytes
		normalizer.count, normalizer.requiresV2, normalizer.invalid, normalizer.rejectPart = control.Count, control.RequiresV2, control.Invalid, control.RejectPart
		work.phase++
		return nil
	}
	if work.phase < 3 {
		ref, model := work.control.Envelope, uint8(legacyNativeCheckpointEnvelopeModel)
		present := work.control.HasEnvelope
		if work.phase == 2 {
			ref, model, present = work.control.Event, legacyNativeCheckpointEventModel, work.control.HasEvent
		}
		if !present {
			work.phase++
			return nil
		}
		if work.child == nil {
			if work.phase == 2 {
				remaining, err := work.remaining()
				if err != nil {
					return err
				}
				var planned docRangeHeaderHeapEstimate
				planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationNativeEvent{})))
				if planned.err != nil || planned.bytes > remaining {
					return ErrDocumentResourceLimit
				}
				work.event = &legacyNormalizationNativeEvent{}
			}
			owner := legacyNativeObjectCheckpointOwner{decoder: work.normalizer, event: work.event}
			shared, err := owner.retainedBytes()
			if err != nil {
				return err
			}
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			var planned docRangeHeaderHeapEstimate
			planned.include(docRangeHeaderHeapEstimate{bytes: remaining})
			planned.include(docRangeHeaderHeapEstimate{bytes: shared})
			if planned.err != nil {
				return planned.err
			}
			work.child, err = newLegacyNativeObjectCheckpointRestoreForOwner(ctx, ref, model, work.store, work.normalizer.source, work.binding, planned.bytes, owner)
			return err
		}
		complete, err := work.child.step(ctx)
		if err != nil || !complete {
			return err
		}
		restored, err := work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
		if work.phase == 1 {
			expected := work.normalizer.frame
			if !work.control.EnvelopeActive {
				expected = legacyJSONValueScanner{}
			}
			if restored.Scanner != expected || restored.Object.parentDepth != 2 || restored.Object.trackPresence || restored.Object.prefix || !work.control.EnvelopeActive && restored.Object.phase != legacyNativeObjectDone || work.control.EnvelopeActive && restored.Object.end != work.normalizer.position {
				return ErrIntegrity
			}
			work.normalizer.envelopeNative = restored.Object
			work.normalizer.envelopeActive = work.control.EnvelopeActive
		} else {
			if restored.Scanner != work.normalizer.frame || restored.Object.parentDepth != 3 || !restored.Object.trackPresence || !restored.Object.pagedEvents {
				return ErrIntegrity
			}
			if err := legacyNormalizationDecoderEventPosition(work.normalizer.position, work.normalizer.source.Bytes, restored.Object); err != nil {
				return err
			}
			work.event.object = restored.Object
			work.normalizer.native, work.normalizer.nativeCache = work.event, work.event
			work.event = nil
		}
		work.normalizer.source = restored.Source
		work.phase++
		return nil
	}
	if work.phase == 3 {
		if work.control.Phase != legacyNormalizationFinalizing {
			work.phase++
			return nil
		}
		if work.header == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			shared, err := work.normalizer.RetainedBytes()
			if err != nil {
				return err
			}
			work.header, err = newLegacyNormalizationHeaderCheckpointRestore(ctx, *work.control.Header, work.normalizer, work.binding, work.store, remaining+shared)
			return err
		}
		ready, err := work.header.step(ctx)
		if err != nil || !ready {
			return err
		}
		header, err := work.header.take(ctx)
		work.header = nil
		if err != nil || header != work.normalizer.finalizing {
			if err == nil {
				err = ErrIntegrity
			}
			return err
		}
		work.phase++
		return nil
	}
	work.ready = true
	return nil
}

func (work *legacyNormalizationDecoderCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if remaining, err := work.remaining(); err != nil || remaining < legacyNativeAnyCheckpointScratch {
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
	if _, err := work.remaining(); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNormalizationDecoderCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.control.heap())
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)), 1)
	retained, err := work.normalizer.RetainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.event != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.event)))
	}
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		if err != nil {
			return 0, err
		}
		shared, err := work.child.owner.retainedBytes()
		if err != nil || shared > retained {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - shared})
	}
	if work.header != nil {
		retained, err := work.header.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationDecoderCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNormalizationDecoderCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNormalizationDecoderCheckpointRestore) take(ctx context.Context) (*LegacyNormalizationDecoder, error) {
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
	if work.closed || !work.ready || work.normalizer.mutation.activity.Load() != 0 {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	normalizer := work.normalizer
	work.normalizer = nil
	work.close()
	return normalizer, nil
}

func (work *legacyNormalizationDecoderCheckpointRestore) close() {
	if work != nil && !work.closed {
		if work.child != nil {
			work.child.close()
		}
		work.header.close()
		if work.normalizer != nil {
			work.normalizer.mutation.stop()
		}
		failure := work.failure
		*work = legacyNormalizationDecoderCheckpointRestore{closed: true, failure: failure}
	}
}
