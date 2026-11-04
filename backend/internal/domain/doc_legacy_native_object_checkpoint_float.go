package domain

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"unsafe"
)

type legacyNativeObjectFloatCheckpointSnapshot struct {
	metadata       *legacyNativeObjectFields
	destination    uintptr
	field          legacyNativeObjectField
	index          int
	position       int
	end            int
	valueStart     int
	valueEnd       int
	phase          uint8
	first          byte
	started        bool
	number         *legacyNativeNumberValue
	numberShape    legacyNativeNumberValue
	valueBits      uint64
	floatShape     legacyNativeFloat64
	decimalShape   legacyNativeFloat64Decimal
	textShape      legacyNativeScalar
	textData       *byte
	textLength     int
	textCapacity   int
	previousData   *byte
	previousLength int
	previousCap    int
	semanticShape  json.UnmarshalTypeError
	semanticData   *byte
	semanticLength int
}

func legacyNativeObjectFloatCheckpointSnapshotFor(native *legacyNativeObject, source LegacyByteSource) (legacyNativeObjectFloatCheckpointSnapshot, error) {
	var snapshot legacyNativeObjectFloatCheckpointSnapshot
	if native == nil || native.metadata == nil || native.metadata.err != nil || len(native.metadata.fields) > 32 ||
		native.number == nil || native.number.parser == nil || native.field < 0 || native.field >= len(native.metadata.fields) ||
		!native.destination.IsValid() || native.destination.Kind() != reflect.Pointer || native.destination.IsNil() ||
		native.destination.Elem().Type() != native.metadata.model {
		return snapshot, ErrIntegrity
	}
	field := native.metadata.fields[native.field]
	if field.index < 0 || field.index >= native.destination.Elem().NumField() || !field.interfaceString ||
		native.destination.Elem().Field(field.index).Type() != reflect.TypeFor[any]() ||
		native.phase != legacyNativeObjectApply || native.skipValue || !native.valueStarted ||
		native.valueStart < 0 || native.valueEnd <= native.valueStart || native.valueEnd > native.end || native.end > source.Bytes ||
		native.position != native.valueEnd || !native.numberField(field) || native.scalar != nil || native.typedNumber != nil ||
		native.listActive || native.blockActive || native.eventActive || native.nestedActive || native.containerActive {
		return snapshot, ErrIntegrity
	}
	number := native.number
	parser := number.parser
	if parser.from != native.valueStart || parser.end != native.valueEnd ||
		parser.authority == 0 && parser.sourceBytes != 0 ||
		parser.authority != 0 && (parser.authority != source.authority || parser.sourceBytes != source.Bytes) {
		return snapshot, ErrIntegrity
	}
	snapshot = legacyNativeObjectFloatCheckpointSnapshot{
		metadata: native.metadata, destination: native.destination.Pointer(), field: field, index: native.field,
		position: native.position, end: native.end, valueStart: native.valueStart, valueEnd: native.valueEnd,
		phase: native.phase, first: native.valueFirst, started: native.valueStarted,
		number: number, numberShape: *number, valueBits: math.Float64bits(number.value), floatShape: *parser,
	}
	if parser.decimal != nil {
		snapshot.decimalShape = *parser.decimal
	}
	if text := number.errorText; text != nil {
		snapshot.textShape = *text
		if text.text != nil {
			snapshot.textData = unsafe.StringData(text.text.String())
			snapshot.textLength, snapshot.textCapacity = text.text.Len(), text.text.Cap()
		}
		if text.previous != nil {
			snapshot.previousData = unsafe.StringData(text.previous.String())
			snapshot.previousLength, snapshot.previousCap = text.previous.Len(), text.previous.Cap()
		}
	}
	if number.semantic != nil {
		snapshot.semanticShape = *number.semantic
		snapshot.semanticShape.Value = ""
		snapshot.semanticData = unsafe.StringData(number.semantic.Value)
		snapshot.semanticLength = len(number.semantic.Value)
	}
	return snapshot, nil
}

type legacyNativeObjectFloatCheckpoint struct {
	native     *legacyNativeObject
	snapshot   legacyNativeObjectFloatCheckpointSnapshot
	binding    ContentHash
	source     LegacyByteSource
	liveSource LegacyByteSource
	store      legacyNativeCheckpointIO
	child      *legacyNativeCheckpointNumberWriter
	sourceHeap docRangeHeaderHeapEstimate
	ref        legacyNativeCheckpointRef
	budget     int64
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeObjectFloatCheckpoint(ctx context.Context, native *legacyNativeObject, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectFloatCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Put == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	snapshot, err := legacyNativeObjectFloatCheckpointSnapshotFor(native, source)
	if err != nil {
		return nil, err
	}
	var sourceHeap docRangeHeaderHeapEstimate
	snapshot.number.retained(&sourceHeap)
	if snapshot.number.semantic != nil {
		sourceHeap.allocation(int64(len(snapshot.number.semantic.Struct)), 1)
		sourceHeap.allocation(int64(len(snapshot.number.semantic.Field)), 1)
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectFloatCheckpoint{})))
	planned.include(sourceHeap)
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(binding)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	owned, err := legacyNativeObjectCheckpointNestedOwnSource(ctx, source)
	if err != nil {
		return nil, err
	}
	work := &legacyNativeObjectFloatCheckpoint{native: native, snapshot: snapshot,
		binding: ContentHash(strings.Clone(string(binding))), source: owned, liveSource: source,
		store: store, sourceHeap: sourceHeap, budget: budget}
	if err := work.checkSource(ctx); err != nil {
		work.close()
		return nil, err
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectFloatCheckpoint) checkSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.native == nil || work.native.number != work.snapshot.number {
		return ErrConflict
	}
	current, err := legacyNativeObjectFloatCheckpointSnapshotFor(work.native, work.source)
	if err != nil {
		return err
	}
	if current != work.snapshot {
		return ErrConflict
	}
	return legacyNativeObjectCheckpointNestedSource(ctx, work.liveSource, work.source)
}

func (work *legacyNativeObjectFloatCheckpoint) step(ctx context.Context) (bool, error) {
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
	if err := work.checkSource(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if !work.ready {
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return work.fail(err)
			}
			work.child, err = newLegacyNativeCheckpointNumberWriter(ctx, work.snapshot.number, work.store, work.source.Bytes, remaining)
			if err != nil {
				return work.fail(err)
			}
			work.child.state.Binding = work.binding
			if err := work.child.bindSource(ctx, work.source); err != nil {
				return work.fail(err)
			}
		} else {
			ready, err := work.child.step(ctx)
			if err != nil {
				return work.fail(err)
			}
			if ready {
				if err := work.checkSource(ctx); err != nil {
					return work.fail(err)
				}
				if err := work.admit(0); err != nil {
					return work.fail(err)
				}
				work.ref, err = work.child.take(ctx)
				if err != nil {
					return work.fail(err)
				}
				work.child = nil
				if !legacyNativeCheckpointBlockListHashValid(work.ref.Hash) || !work.ref.valid() || work.ref.Bytes > legacyNativeCheckpointNumberMetadataBytes {
					return work.fail(ErrIntegrity)
				}
				work.ready = true
			}
		}
	}
	if err := work.checkSource(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectFloatCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.liveSource))
	heap.allocation(int64(len(work.binding)), 1)
	heap.allocation(int64(len(work.ref.Hash)), 1)
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectFloatCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if work.sourceHeap.err != nil {
		return 0, work.sourceHeap.err
	}
	if retained < 0 || retained > work.budget || work.sourceHeap.bytes < 0 || work.sourceHeap.bytes > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - work.sourceHeap.bytes, nil
}

func (work *legacyNativeObjectFloatCheckpoint) admit(extra int64) error {
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

func (work *legacyNativeObjectFloatCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, work.failure
	}
	if work.closed || !work.ready || !work.ref.valid() {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := work.checkSource(ctx); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, work.failure
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeObjectFloatCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectFloatCheckpoint) close() {
	if work != nil {
		if work.child != nil {
			work.child.close()
		}
		failure := work.failure
		*work = legacyNativeObjectFloatCheckpoint{closed: true, failure: failure}
	}
}

type legacyNativeObjectFloatCheckpointRestore struct {
	ref        legacyNativeCheckpointRef
	binding    ContentHash
	source     LegacyByteSource
	liveSource LegacyByteSource
	store      legacyNativeCheckpointIO
	child      *legacyNativeCheckpointNumberRestore
	result     legacyNativeObjectFloatRestored
	budget     int64
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeObjectFloatCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectFloatCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Read == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) ||
		!legacyNativeCheckpointBlockListHashValid(ref.Hash) || !ref.valid() || ref.Bytes > legacyNativeCheckpointNumberMetadataBytes {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectFloatCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(binding)+len(ref.Hash)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	owned, err := legacyNativeObjectCheckpointNestedOwnSource(ctx, source)
	if err != nil {
		return nil, err
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	work := &legacyNativeObjectFloatCheckpointRestore{ref: ref, binding: ContentHash(strings.Clone(string(binding))),
		source: owned, liveSource: source, store: store, budget: budget}
	if err := work.checkSource(ctx); err != nil {
		work.close()
		return nil, err
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectFloatCheckpointRestore) checkSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := legacyNativeObjectCheckpointNestedSource(ctx, work.liveSource, work.source); err != nil {
		return err
	}
	if work.ready {
		if work.result.Number == nil || work.result.Number.parser == nil {
			return ErrIntegrity
		}
		parser := work.result.Number.parser
		if parser.authority == 0 && parser.sourceBytes != 0 ||
			parser.authority != 0 && (parser.authority != work.source.authority || parser.sourceBytes != work.source.Bytes) {
			return ErrIntegrity
		}
		return legacyNativeObjectCheckpointNestedSource(ctx, work.result.Source, work.source)
	}
	return ctx.Err()
}

func (work *legacyNativeObjectFloatCheckpointRestore) advance(ctx context.Context) error {
	if work.child == nil {
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		work.child, err = newLegacyNativeCheckpointNumberRestore(ctx, work.ref, work.store, work.source, remaining)
		return err
	}
	ready, err := work.child.step(ctx)
	if work.child.state.Version != 0 && work.child.state.Binding != work.binding {
		return ErrIntegrity
	}
	if err != nil || !ready {
		return err
	}
	if err := work.checkSource(ctx); err != nil {
		return err
	}
	if err := work.admit(0); err != nil {
		return err
	}
	resumed := work.child.source()
	if err := legacyNativeObjectCheckpointNestedSource(ctx, resumed, work.source); err != nil {
		return err
	}
	number, err := work.child.take(ctx)
	if err != nil {
		return err
	}
	work.child = nil
	work.result = legacyNativeObjectFloatRestored{Number: number, Source: resumed}
	work.ready = true
	return nil
}

func (work *legacyNativeObjectFloatCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.checkSource(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if !work.ready {
		if err := work.advance(ctx); err != nil {
			return work.fail(err)
		}
	}
	if err := work.checkSource(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectFloatCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.liveSource))
	heap.allocation(int64(len(work.binding)+len(work.ref.Hash)), 1)
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.result.Number != nil {
		work.result.Number.retained(&heap)
		if work.result.Number.semantic != nil {
			heap.allocation(int64(len(work.result.Number.semantic.Struct)), 1)
			heap.allocation(int64(len(work.result.Number.semantic.Field)), 1)
		}
	}
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.result.Source))
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectFloatCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeObjectFloatCheckpointRestore) admit(extra int64) error {
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

func (work *legacyNativeObjectFloatCheckpointRestore) take(ctx context.Context) (legacyNativeObjectFloatRestored, error) {
	if work == nil {
		return legacyNativeObjectFloatRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectFloatRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectFloatRestored{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectFloatRestored{}, work.failure
	}
	if err := work.checkSource(ctx); err != nil {
		work.fail(err)
		return legacyNativeObjectFloatRestored{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectFloatRestored{}, work.failure
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeObjectFloatCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectFloatCheckpointRestore) close() {
	if work != nil {
		if work.child != nil {
			work.child.close()
		}
		failure := work.failure
		*work = legacyNativeObjectFloatCheckpointRestore{closed: true, failure: failure}
	}
}
