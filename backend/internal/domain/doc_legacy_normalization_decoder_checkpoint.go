package domain

import (
	"context"
	"reflect"
	"strings"
	"unsafe"
)

type legacyNormalizationDecoderControl struct {
	Kind           string                     `json:"kind"`
	Version        uint8                      `json:"version"`
	Binding        ContentHash                `json:"binding"`
	SourceBytes    int                        `json:"source_bytes"`
	PartBytes      int                        `json:"part_bytes"`
	Position       int                        `json:"position"`
	Phase          uint8                      `json:"phase"`
	Frame          []byte                     `json:"frame"`
	ValueStart     int                        `json:"value_start"`
	Field          uint8                      `json:"field"`
	Key            [64]byte                   `json:"key"`
	KeyBytes       int                        `json:"key_bytes"`
	Count          int                        `json:"count"`
	RequiresV2     bool                       `json:"requires_v2"`
	Invalid        bool                       `json:"invalid"`
	RejectPart     int                        `json:"reject_part"`
	HasEnvelope    bool                       `json:"has_envelope"`
	EnvelopeActive bool                       `json:"envelope_active"`
	Envelope       legacyNativeCheckpointRef  `json:"envelope"`
	HasEvent       bool                       `json:"has_event"`
	Event          legacyNativeCheckpointRef  `json:"event"`
	Header         *legacyNativeCheckpointRef `json:"header,omitempty"`
}

func (control legacyNormalizationDecoderControl) validate(ctx context.Context, binding ContentHash, source LegacyByteSource, ready bool) error {
	if control.Kind != "legacy-normalization-decoder" || control.Version != 1 && control.Version != 2 || (control.Version == 2) != (control.Phase == legacyNormalizationFinalizing) || control.Binding != binding || ValidateContentHash(binding) != nil ||
		control.SourceBytes != source.Bytes || control.PartBytes != source.partBytes() || control.Phase > legacyNormalizationFinalizing ||
		control.Position < 0 || control.Position > source.Bytes || control.ValueStart < 0 || control.ValueStart > control.Position ||
		control.Field > legacyNormalizationEventsField || control.KeyBytes < 0 || control.KeyBytes > len(control.Key)+1 ||
		control.Count < 0 || control.Count > MaxFinalizedDocBytes || control.RejectPart < 0 || control.RejectPart > len(source.Parts) ||
		control.EnvelopeActive && (!control.HasEnvelope || control.Phase != legacyNormalizationRootNext) ||
		control.HasEvent && (control.Phase != legacyNormalizationEvent || control.EnvelopeActive) ||
		control.Phase == legacyNormalizationReject && (control.HasEnvelope || control.HasEvent) ||
		control.RejectPart != 0 && control.Phase != legacyNormalizationReject {
		return ErrIntegrity
	}
	if control.Phase == legacyNormalizationFinalizing {
		if control.Position != source.Bytes || control.Invalid || control.HasEnvelope || control.HasEvent || control.EnvelopeActive || ready && (control.Header == nil || !control.Header.valid()) {
			return ErrIntegrity
		}
	} else if control.Header != nil {
		return ErrIntegrity
	}
	if _, err := restoreLegacyJSONValueScanner(ctx, control.Frame); err != nil {
		return err
	}
	for _, reference := range []struct {
		present bool
		ref     legacyNativeCheckpointRef
	}{{control.HasEnvelope, control.Envelope}, {control.HasEvent, control.Event}} {
		if !reference.present && reference.ref != (legacyNativeCheckpointRef{}) || ready && reference.present && !reference.ref.valid() {
			return ErrIntegrity
		}
	}
	return nil
}

func (control legacyNormalizationDecoderControl) heap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	for _, text := range []string{control.Kind, string(control.Binding), string(control.Envelope.Hash), string(control.Event.Hash)} {
		heap.allocation(int64(len(text)), 1)
	}
	if control.Header != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*control.Header)))
		heap.allocation(int64(len(control.Header.Hash)), 1)
	}
	heap.allocation(int64(cap(control.Frame)), 1)
	return heap
}

type legacyNormalizationDecoderCheckpoint struct {
	normalizer *LegacyNormalizationDecoder
	capture    *legacyNormalizationCapture
	control    legacyNormalizationDecoderControl
	store      legacyNativeCheckpointIO
	child      *legacyNativeObjectCheckpoint
	header     *legacyNormalizationHeaderCheckpoint
	idleFrame  legacyJSONValueScanner
	root       legacyNativeCheckpointRef
	budget     int64
	phase      uint8
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNormalizationDecoderCheckpoint(ctx context.Context, normalizer *LegacyNormalizationDecoder, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (_ *legacyNormalizationDecoderCheckpoint, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer == nil || store.Put == nil || budget <= 0 || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil {
		return nil, ErrIntegrity
	}
	var minimum docRangeHeaderHeapEstimate
	minimum.allocation(1, int64(unsafe.Sizeof(legacyNormalizationDecoderCheckpoint{})))
	minimum.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	minimum.allocation(legacyNativeAnyCheckpointScratch, 1)
	if minimum.err != nil || minimum.bytes > budget {
		return nil, ErrDocumentResourceLimit
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
	if normalizer.terminal != nil {
		return nil, normalizer.terminal
	}
	if normalizer.phase > legacyNormalizationFinalizing {
		return nil, ErrDocRangeIndexPending
	}
	if (normalizer.finalizing != nil) != (normalizer.phase == legacyNormalizationFinalizing) || len(normalizer.header) != 0 || normalizer.source.authority == 0 || normalizer.source.Read == nil {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(normalizer.source, normalizer.source.Bytes); err != nil {
		return nil, err
	}
	retained, err := normalizer.RetainedBytes()
	minimum.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	minimum.allocation(legacyJSONCheckpointHeader+int64(normalizer.frame.depth), 1)
	minimum.allocation(int64(len(binding)), 1)
	if minimum.err != nil || minimum.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	frame, err := normalizer.frame.checkpoint(ctx)
	if err != nil {
		return nil, err
	}
	control := legacyNormalizationDecoderControl{Kind: "legacy-normalization-decoder", Version: 1, Binding: ContentHash(strings.Clone(string(binding))),
		SourceBytes: normalizer.source.Bytes, PartBytes: normalizer.source.partBytes(), Position: normalizer.position, Phase: normalizer.phase, Frame: frame,
		ValueStart: normalizer.valueStart, Field: normalizer.field, Key: normalizer.key, KeyBytes: normalizer.keyBytes,
		Count: normalizer.count, RequiresV2: normalizer.requiresV2, Invalid: normalizer.invalid, RejectPart: normalizer.rejectPart,
		HasEnvelope: normalizer.envelopeNative != nil, EnvelopeActive: normalizer.envelopeActive, HasEvent: normalizer.native != nil}
	if normalizer.phase == legacyNormalizationFinalizing {
		control.Version = 2
	}
	if !control.HasEnvelope && !reflect.ValueOf(normalizer.envelope).IsZero() || control.HasEvent && normalizer.native != normalizer.nativeCache {
		return nil, ErrIntegrity
	}
	if normalizer.envelopeNative != nil && (normalizer.envelopeNative.mutation != normalizer.mutation || normalizer.envelopeNative.destination.Interface() != &normalizer.envelope || normalizer.envelopeNative.metadata != &legacyEnvelopeNativeFields || normalizer.envelopeNative.parentDepth != 2 || normalizer.envelopeNative.trackPresence || normalizer.envelopeNative.prefix || !normalizer.envelopeActive && normalizer.envelopeNative.phase != legacyNativeObjectDone) {
		return nil, ErrIntegrity
	}
	if normalizer.nativeCache != nil && (normalizer.nativeCache.object == nil || normalizer.nativeCache.canonicalReady || normalizer.nativeCache.object.mutation != normalizer.mutation || normalizer.nativeCache.object.destination.Interface() != &normalizer.nativeCache.decoded || normalizer.nativeCache.object.metadata != &legacyEventNativeFields || normalizer.nativeCache.object.parentDepth != 3 || !normalizer.nativeCache.object.trackPresence || !control.HasEvent && !reflect.ValueOf(normalizer.nativeCache.decoded).IsZero()) {
		return nil, ErrIntegrity
	}
	if control.HasEvent {
		native := normalizer.native.object
		if err := legacyNormalizationDecoderEventPosition(normalizer.position, normalizer.source.Bytes, native); err != nil {
			return nil, err
		}
	}
	if !control.HasEvent && normalizer.nativeCache != nil {
		native := normalizer.nativeCache.object
		if native.phase != legacyNativeObjectStart || native.position != 0 || native.end != 0 || native.lists != nil || native.blocks != nil || native.events != nil || native.nested != nil || native.containers != nil || native.scalar != nil || native.number != nil || native.typedNumber != nil || native.typeError != nil || native.presence != 0 || len(native.key) != 0 || len(native.raw) != 0 {
			return nil, ErrIntegrity
		}
	}
	if err := control.validate(ctx, binding, normalizer.source, false); err != nil {
		return nil, err
	}
	return &legacyNormalizationDecoderCheckpoint{normalizer: normalizer, capture: capture, control: control, store: store, budget: budget}, nil
}

func legacyNormalizationDecoderEventPosition(position, sourceBytes int, native *legacyNativeObject) error {
	if native.phase == legacyNativeObjectDone {
		return ErrIntegrity
	}
	expected := native.end
	if native.prefix {
		if native.end != sourceBytes {
			return ErrIntegrity
		}
		expected = native.progressPosition()
	}
	if position != expected {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNormalizationDecoderCheckpoint) advance(ctx context.Context) error {
	if work.phase < 2 {
		native, frame := work.normalizer.envelopeNative, &work.normalizer.frame
		if work.phase == 1 {
			if !work.control.HasEvent {
				work.phase++
				return nil
			}
			native = work.normalizer.native.object
		} else if !work.control.HasEnvelope {
			work.phase++
			return nil
		} else if !work.control.EnvelopeActive {
			frame = &work.idleFrame
		}
		if work.child == nil {
			retained, err := native.retained()
			if err != nil {
				return err
			}
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.child, err = newLegacyNativeObjectCheckpoint(ctx, native, frame, work.control.Binding, work.normalizer.source, work.store, remaining+retained)
			return err
		}
		complete, err := work.child.step(ctx)
		if err != nil || !complete {
			return err
		}
		ref, err := work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
		if work.phase == 0 {
			work.control.Envelope = ref
		} else {
			work.control.Event = ref
		}
		work.phase++
		return nil
	}
	if work.phase == 2 {
		if work.control.Phase != legacyNormalizationFinalizing {
			work.phase++
			return nil
		}
		if work.header == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.header, err = newLegacyNormalizationHeaderCheckpoint(ctx, work.normalizer, work.control.Binding, work.store, remaining)
			return err
		}
		ready, err := work.header.step(ctx)
		if err != nil || !ready {
			return err
		}
		ref, err := work.header.take(ctx)
		work.header = nil
		if err != nil {
			return err
		}
		work.control.Header = &ref
		work.phase++
		return nil
	}
	if err := work.control.validate(ctx, work.control.Binding, work.normalizer.source, true); err != nil {
		return err
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.control)
	if err != nil {
		return err
	}
	work.root, work.ready = ref, true
	return nil
}

func (work *legacyNormalizationDecoderCheckpoint) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed || !work.capture.valid(work.normalizer.mutation) {
		return work.fail(ErrIntegrity)
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

func (work *legacyNormalizationDecoderCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, int64(unsafe.Sizeof(*work.capture)))
	heap.include(work.control.heap())
	heap.allocation(int64(len(work.root.Hash)), 1)
	retained, err := work.normalizer.RetainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.header != nil {
		retained, err := work.header.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationDecoderCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNormalizationDecoderCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNormalizationDecoderCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	if work.closed || !work.ready || !work.capture.valid(work.normalizer.mutation) {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	result := work.root
	work.close()
	return result, nil
}

func (work *legacyNormalizationDecoderCheckpoint) close() {
	if work != nil && !work.closed {
		if work.child != nil {
			work.child.close()
		}
		work.header.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNormalizationDecoderCheckpoint{closed: true, failure: failure}
	}
}
