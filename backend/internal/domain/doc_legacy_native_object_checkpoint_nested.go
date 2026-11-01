package domain

import (
	"context"
	"reflect"
	"strings"
	"unsafe"
)

func (state legacyNativeObjectCheckpointNestedField) validate(expectedModel uint8, present, initialized bool) error {
	if expectedModel != legacyNativeCheckpointLockedModel && expectedModel != legacyNativeCheckpointProviderModel || state.Model != expectedModel {
		return ErrIntegrity
	}
	if !present {
		if state.Value != nil || state.Parser != (legacyNativeCheckpointRef{}) {
			return ErrIntegrity
		}
		return nil
	}
	if state.Parser != (legacyNativeCheckpointRef{}) {
		if state.Value != nil || len(state.Parser.Hash) != len(contentHashPrefix)+64 || !state.Parser.valid() {
			return ErrIntegrity
		}
		return nil
	}
	if state.Value == nil || !initialized {
		return ErrIntegrity
	}
	return state.Value.validate(expectedModel)
}

func legacyNativeObjectCheckpointNestedModel(model reflect.Type) uint8 {
	switch model {
	case reflect.TypeFor[*LockedBlob]():
		return legacyNativeCheckpointLockedModel
	case reflect.TypeFor[*ProviderMetadata]():
		return legacyNativeCheckpointProviderModel
	default:
		return 0
	}
}

func legacyNativeObjectCheckpointNestedFieldHeap(state legacyNativeObjectCheckpointNestedField) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(len(state.Parser.Hash)), 1)
	if state.Value != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*state.Value)))
		heap.allocation(int64(len(state.Value.Schema)+len(state.Value.Values.Items.Ref.Hash)), 1)
	}
	return heap
}

func legacyNativeObjectCheckpointNestedParserHeap(parser *legacyNativeObject) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if parser != nil {
		if parser.metadata == nil || !parser.destination.IsValid() || parser.destination.Kind() != reflect.Pointer || parser.destination.IsNil() || parser.destination.Elem().Type() != parser.metadata.model {
			return docRangeHeaderHeapEstimate{err: ErrIntegrity}
		}
		retained, err := parser.retained()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		heap.allocation(1, int64(parser.destination.Type().Size()))
		heap.allocation(1, int64(parser.metadata.model.Size()))
	}
	return heap
}

func legacyNativeObjectCheckpointNestedSource(ctx context.Context, current, expected LegacyByteSource) error {
	if err := legacyNativeBlockListCheckpointSource(ctx, current); err != nil {
		return err
	}
	if current.authority != expected.authority || current.Bytes != expected.Bytes || current.partBytes() != expected.partBytes() || len(current.Parts) != len(expected.Parts) {
		return ErrIntegrity
	}
	for index, part := range current.Parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if part != expected.Parts[index] {
			return ErrIntegrity
		}
	}
	return ctx.Err()
}

func legacyNativeObjectCheckpointNestedOwnSource(ctx context.Context, source LegacyByteSource) (LegacyByteSource, error) {
	parts := make([]ConversationManifestChunk, len(source.Parts))
	copy(parts, source.Parts)
	source.Parts = parts
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return LegacyByteSource{}, err
		}
		source.Parts[index].Hash = ContentHash(strings.Clone(string(source.Parts[index].Hash)))
	}
	return source, ctx.Err()
}

func legacyNativeObjectCheckpointNestedChild(parser *legacyNativeObject, model uint8, scanner *legacyJSONValueScanner, binding ContentHash, sourceBytes int) error {
	if parser == nil || scanner == nil || scanner.invalid {
		return ErrIntegrity
	}
	actual, err := legacyNativeCheckpointModel(parser.metadata)
	if err != nil || actual != model || model != legacyNativeCheckpointLockedModel && model != legacyNativeCheckpointProviderModel ||
		!parser.destination.IsValid() || parser.destination.Kind() != reflect.Pointer || parser.destination.IsNil() ||
		parser.destination.Type() != reflect.PointerTo(parser.metadata.model) || cap(parser.key) != parser.metadata.keyBytes ||
		len(parser.heaps) != len(parser.metadata.fields) || len(parser.backingExcess) != len(parser.metadata.fields) ||
		parser.parentDepth < 2 || parser.parentDepth > 10000 || !parser.prefix || !parser.deferTypeErrors ||
		parser.trackPresence || parser.presence != 0 || parser.pagedEvents || parser.number != nil ||
		len(parser.lists) != 0 || len(parser.blocks) != 0 || len(parser.events) != 0 || len(parser.nested) != 0 || len(parser.containers) != 0 ||
		parser.listActive || parser.blockActive || parser.eventActive || parser.nestedActive || parser.containerActive {
		return ErrIntegrity
	}
	state := legacyNativeObjectCheckpointControlFor(parser, model, binding, sourceBytes)
	if err := state.validate(model, binding, sourceBytes, false); err != nil {
		return err
	}
	if err := legacyNativeObjectCheckpointScanner(state, scanner); err != nil {
		return err
	}
	if !scanner.complete {
		if err := scanner.validateCheckpoint(); err != nil {
			return err
		}
	}
	if *scanner == (legacyJSONValueScanner{}) && parser.phase != legacyNativeObjectDone || parser.checkpointScanner != nil && *parser.checkpointScanner != *scanner {
		return ErrIntegrity
	}
	for index := range parser.metadata.fields {
		if _, err := legacyNativeObjectCheckpointFieldValue(parser, index); err != nil {
			return err
		}
	}
	return nil
}

type legacyNativeObjectNestedCheckpointSnapshot struct {
	metadata       *legacyNativeObjectFields
	destination    uintptr
	pointer        uintptr
	number         uintptr
	values         [3]string
	heap           docRangeHeaderHeapEstimate
	excess         int64
	cache          **legacyNativeObject
	cacheCapacity  int
	checkpoint     *legacyJSONValueScanner
	active         bool
	nestedActive   bool
	root           legacyNativeObjectCheckpointControl
	parser         *legacyNativeObject
	parserMetadata *legacyNativeObjectFields
	parserScanner  *legacyJSONValueScanner
	parserState    legacyNativeObjectCheckpointControl
	guards         [3]legacyNativeObjectCheckpointValueGuard
	scalar         *legacyNativeScalar
	typedNumber    *legacyNativeTypedNumber
	semantic       error
}

func legacyNativeObjectNestedCheckpointSnapshotFor(native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, sourceBytes int) (legacyNativeObjectNestedCheckpointSnapshot, reflect.Value, uint8, error) {
	var snapshot legacyNativeObjectNestedCheckpointSnapshot
	if native == nil || scanner == nil || scanner.invalid || native.metadata == nil ||
		!native.destination.IsValid() || native.destination.Kind() != reflect.Pointer || native.destination.IsNil() ||
		native.destination.Elem().Type() != native.metadata.model || field < 0 || field >= len(native.metadata.fields) ||
		len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) ||
		native.nested != nil && len(native.nested) != len(native.metadata.fields) {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	parentModel, err := legacyNativeCheckpointModel(native.metadata)
	if err != nil || parentModel != legacyNativeCheckpointEventModel || native.parentDepth < 1 || native.parentDepth > 10000 ||
		native.position < 0 || native.position > native.end || native.end <= 0 || native.end > sourceBytes ||
		cap(native.key) != native.metadata.keyBytes || cap(native.raw) > native.metadata.rawBytes ||
		native.field < -1 || native.field >= len(native.metadata.fields) || native.phase > legacyNativeObjectDone ||
		native.checkpointScanner != nil && *native.checkpointScanner != *scanner {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	if _, err := legacyNativeCheckpointModelSchema(native.metadata); err != nil {
		return snapshot, reflect.Value{}, 0, err
	}
	metadata := native.metadata.fields[field]
	if !metadata.nestedObject || metadata.index < 0 || metadata.index >= native.destination.Elem().NumField() {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	value := native.destination.Elem().Field(metadata.index)
	model := legacyNativeObjectCheckpointNestedModel(value.Type())
	if model == 0 {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	if !scanner.complete {
		if err := scanner.validateCheckpoint(); err != nil {
			return snapshot, reflect.Value{}, 0, err
		}
	}
	snapshot = legacyNativeObjectNestedCheckpointSnapshot{
		metadata: native.metadata, destination: native.destination.Pointer(), pointer: value.Pointer(),
		heap: native.heaps[field], excess: native.backingExcess[field], cache: unsafe.SliceData(native.nested), cacheCapacity: cap(native.nested),
		checkpoint: native.checkpointScanner, active: native.nestedActive && native.field == field, nestedActive: native.nestedActive,
		root: legacyNativeObjectCheckpointControlFor(native, parentModel, binding, sourceBytes),
	}
	if snapshot.heap.err != nil || snapshot.heap.bytes < 0 || snapshot.excess < 0 || snapshot.excess%2 != 0 ||
		snapshot.heap.bytes == 0 && snapshot.excess != 0 || value.IsNil() && snapshot.excess != 0 {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	if !value.IsNil() {
		for index, member := range legacyNativeCheckpointModelMetadata(model).fields {
			current := value.Elem().Field(member.index)
			if current.Kind() == reflect.String {
				snapshot.values[index] = current.String()
			} else if current.Kind() == reflect.Pointer && !current.IsNil() {
				snapshot.number = current.Pointer()
				snapshot.values[index] = current.Elem().String()
			}
		}
	}
	if native.nested != nil {
		snapshot.parser = native.nested[field]
	}
	if snapshot.parser == nil {
		if snapshot.active || snapshot.excess != 0 || !value.IsNil() && snapshot.heap.bytes == 0 {
			return snapshot, reflect.Value{}, 0, ErrIntegrity
		}
		return snapshot, value, model, nil
	}
	parser := snapshot.parser
	if value.IsNil() || !parser.destination.IsValid() || parser.destination.Type() != value.Type() || parser.destination.IsNil() ||
		parser.destination.Pointer() != value.Pointer() || parser.parentDepth != native.parentDepth+1 {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	var empty legacyJSONValueScanner
	frame := &empty
	if snapshot.active {
		if native.phase != legacyNativeObjectApply || native.skipValue || !native.valueStarted || native.valueFirst != '{' ||
			native.position != native.valueStart || native.valueStart < 0 || native.valueStart >= native.end || native.valueEnd != native.end ||
			parser.position < native.valueStart || parser.position > native.end || parser.end != native.end || parser.fieldLimit != native.fieldActionLimit() ||
			parser.checkpointScanner != nil && parser.checkpointScanner != scanner && parser.checkpointScanner != native.checkpointScanner {
			return snapshot, reflect.Value{}, 0, ErrIntegrity
		}
		frame = scanner
	} else if parser.phase != legacyNativeObjectDone {
		return snapshot, reflect.Value{}, 0, ErrIntegrity
	}
	if err := legacyNativeObjectCheckpointNestedChild(parser, model, frame, binding, sourceBytes); err != nil {
		return snapshot, reflect.Value{}, 0, err
	}
	snapshot.parserMetadata, snapshot.parserScanner = parser.metadata, parser.checkpointScanner
	snapshot.parserState = legacyNativeObjectCheckpointControlFor(parser, model, binding, sourceBytes)
	snapshot.scalar, snapshot.typedNumber = parser.scalar, parser.typedNumber
	if parser.typeError != nil {
		snapshot.semantic = parser.typeError
	}
	for index := range parser.metadata.fields {
		snapshot.guards[index], err = legacyNativeObjectCheckpointFieldValue(parser, index)
		if err != nil {
			return snapshot, reflect.Value{}, 0, err
		}
	}
	return snapshot, value, model, nil
}

type legacyNativeObjectNestedCheckpoint struct {
	native       *legacyNativeObject
	scanner      *legacyJSONValueScanner
	scannerShape legacyJSONValueScanner
	emptyScanner legacyJSONValueScanner
	snapshot     legacyNativeObjectNestedCheckpointSnapshot
	value        reflect.Value
	state        legacyNativeObjectCheckpointNestedField
	binding      ContentHash
	source       LegacyByteSource
	store        legacyNativeCheckpointIO
	child        *legacyNativeObjectCheckpoint
	independent  legacyNativeAnyCheckpointTask[legacyNativeCheckpointNestedValue]
	sourceHeap   docRangeHeaderHeapEstimate
	field        int
	budget       int64
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeObjectNestedCheckpoint(ctx context.Context, native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectNestedCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Put == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	snapshot, value, model, err := legacyNativeObjectNestedCheckpointSnapshotFor(native, field, scanner, binding, source.Bytes)
	if err != nil {
		return nil, err
	}
	sourceHeap, err := legacyNativeValueHeap(ctx, value)
	if err != nil {
		return nil, err
	}
	if snapshot.parser != nil {
		sourceHeap = legacyNativeObjectCheckpointNestedParserHeap(snapshot.parser)
	} else if snapshot.heap.bytes != 0 && snapshot.heap.bytes != sourceHeap.bytes {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectNestedCheckpoint{})))
	planned.include(sourceHeap)
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.include(legacyNativeObjectCheckpointControlHeap(snapshot.root))
	planned.include(legacyNativeObjectCheckpointControlHeap(snapshot.parserState))
	planned.allocation(int64(len(binding)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	source, err = legacyNativeObjectCheckpointNestedOwnSource(ctx, source)
	if err != nil {
		return nil, err
	}
	rootKey := make([]byte, len(snapshot.root.Key))
	copy(rootKey, snapshot.root.Key)
	snapshot.root.Key = rootKey
	if snapshot.parserState.Key != nil {
		childKey := make([]byte, len(snapshot.parserState.Key))
		copy(childKey, snapshot.parserState.Key)
		snapshot.parserState.Key = childKey
	}
	work := &legacyNativeObjectNestedCheckpoint{native: native, scanner: scanner, scannerShape: *scanner, snapshot: snapshot, value: value,
		state: legacyNativeObjectCheckpointNestedField{Model: model}, binding: ContentHash(strings.Clone(string(binding))),
		source: source, store: store, sourceHeap: sourceHeap, field: field, budget: budget}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectNestedCheckpoint) checkSource() error {
	if work.scanner == nil || *work.scanner != work.scannerShape {
		return ErrConflict
	}
	current, _, model, err := legacyNativeObjectNestedCheckpointSnapshotFor(work.native, work.field, work.scanner, work.binding, work.source.Bytes)
	if err != nil {
		return err
	}
	if model != work.state.Model || !reflect.DeepEqual(current, work.snapshot) {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeObjectNestedCheckpoint) advance(ctx context.Context) error {
	if work.snapshot.parser != nil {
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			frame := &work.emptyScanner
			if work.snapshot.active {
				frame = work.scanner
			}
			work.child, err = newLegacyNativeObjectCheckpoint(ctx, work.snapshot.parser, frame, work.binding, work.source, work.store, remaining)
			return err
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		if err := work.admit(0); err != nil {
			return err
		}
		work.state.Parser, err = work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
	} else if !work.value.IsNil() {
		if work.independent == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointNestedValueWriter(ctx, work.value, work.state.Model, work.store, remaining)
			if err != nil {
				return err
			}
			work.independent = current
			return nil
		}
		ready, err := work.independent.step(ctx)
		if err != nil || !ready {
			return err
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointNestedValue{})))
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		saved, err := work.independent.take(ctx)
		work.independent = nil
		if err != nil {
			return err
		}
		work.state.Value = &saved
	}
	if err := work.state.validate(work.state.Model, !work.value.IsNil(), work.snapshot.heap.bytes != 0); err != nil {
		return err
	}
	work.ready = true
	return nil
}

func (work *legacyNativeObjectNestedCheckpoint) step(ctx context.Context) (bool, error) {
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
	if err := work.checkSource(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectNestedCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.binding)), 1)
	heap.include(legacyNativeObjectCheckpointControlHeap(work.snapshot.root))
	heap.include(legacyNativeObjectCheckpointControlHeap(work.snapshot.parserState))
	heap.include(legacyNativeObjectCheckpointNestedFieldHeap(work.state))
	retained, err := work.child.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.independent != nil {
		retained, err = work.independent.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectNestedCheckpoint) remaining() (int64, error) {
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

func (work *legacyNativeObjectNestedCheckpoint) admit(extra int64) error {
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

func (work *legacyNativeObjectNestedCheckpoint) take(ctx context.Context) (legacyNativeObjectCheckpointNestedField, error) {
	if work == nil {
		return legacyNativeObjectCheckpointNestedField{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectCheckpointNestedField{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointNestedField{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectCheckpointNestedField{}, work.failure
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointNestedField{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointNestedField{}, work.failure
	}
	state := work.state
	work.close()
	return state, nil
}

func (work *legacyNativeObjectNestedCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectNestedCheckpoint) close() {
	if work != nil {
		work.child.close()
		if work.independent != nil {
			work.independent.close()
		}
		failure := work.failure
		*work = legacyNativeObjectNestedCheckpoint{closed: true, failure: failure}
	}
}

type legacyNativeObjectNestedCheckpointRestore struct {
	state       legacyNativeObjectCheckpointNestedField
	initialized bool
	present     bool
	binding     ContentHash
	source      LegacyByteSource
	store       legacyNativeCheckpointIO
	result      legacyNativeObjectNestedRestored
	physical    docRangeHeaderHeapEstimate
	child       *legacyNativeObjectCheckpointRestore
	independent legacyNativeAnyCheckpointTask[reflect.Value]
	budget      int64
	ready       bool
	closed      bool
	failure     error
}

func newLegacyNativeObjectNestedCheckpointRestore(ctx context.Context, saved legacyNativeObjectCheckpointNestedField, initialized, present bool, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectNestedCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Read == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := saved.validate(saved.Model, present, initialized); err != nil {
		return nil, err
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	metadata := legacyNativeCheckpointModelMetadata(saved.Model)
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectNestedCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.include(legacyNativeObjectCheckpointNestedFieldHeap(saved))
	planned.allocation(int64(len(binding)), 1)
	planned.allocation(1, int64(unsafe.Sizeof((*LockedBlob)(nil))))
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	source, err := legacyNativeObjectCheckpointNestedOwnSource(ctx, source)
	if err != nil {
		return nil, err
	}
	saved.Parser.Hash = ContentHash(strings.Clone(string(saved.Parser.Hash)))
	if saved.Value != nil {
		value := *saved.Value
		value.Schema = ContentHash(strings.Clone(string(value.Schema)))
		value.Values.Items.Ref.Hash = ContentHash(strings.Clone(string(value.Values.Items.Ref.Hash)))
		saved.Value = &value
	}
	work := &legacyNativeObjectNestedCheckpointRestore{state: saved, initialized: initialized, present: present,
		binding: ContentHash(strings.Clone(string(binding))), source: source, store: store, budget: budget}
	work.result.Value = reflect.Zero(reflect.PointerTo(metadata.model))
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectNestedCheckpointRestore) finish(ctx context.Context) error {
	value := work.result.Value
	metadata := legacyNativeCheckpointModelMetadata(work.state.Model)
	if !value.IsValid() || value.Type() != reflect.PointerTo(metadata.model) || value.IsNil() == work.present {
		return ErrIntegrity
	}
	heap, err := legacyNativeValueHeap(ctx, value)
	if err != nil {
		return err
	}
	physical := heap
	var excess int64
	if parser := work.result.Parser; parser != nil {
		if work.state.Parser == (legacyNativeCheckpointRef{}) || parser.destination.Type() != value.Type() || parser.destination.Pointer() != value.Pointer() {
			return ErrIntegrity
		}
		if err := legacyNativeObjectCheckpointNestedChild(parser, work.state.Model, &work.result.Scanner, work.binding, work.source.Bytes); err != nil {
			return err
		}
		excess, err = parser.backingExcessBytes(nil)
		if err != nil || excess < 0 || excess%2 != 0 {
			if err != nil {
				return err
			}
			return ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: excess})
		physical = legacyNativeObjectCheckpointNestedParserHeap(parser)
	} else if work.state.Parser != (legacyNativeCheckpointRef{}) || work.result.Scanner != (legacyJSONValueScanner{}) {
		return ErrIntegrity
	}
	if heap.err != nil {
		return heap.err
	}
	if physical.err != nil {
		return physical.err
	}
	work.physical = physical
	work.result.Heap, work.result.Excess = docRangeHeaderHeapEstimate{}, 0
	if work.initialized {
		work.result.Heap, work.result.Excess = heap, excess
	}
	work.ready = true
	return nil
}

func (work *legacyNativeObjectNestedCheckpointRestore) advance(ctx context.Context) error {
	if work.state.Parser != (legacyNativeCheckpointRef{}) {
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.child, err = newLegacyNativeObjectCheckpointRestore(ctx, work.state.Parser, work.state.Model, work.store, work.source, work.binding, remaining)
			return err
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		var transfer docRangeHeaderHeapEstimate
		transfer.allocation(1, int64(work.child.native.destination.Type().Size()))
		if transfer.err != nil {
			return transfer.err
		}
		if err := work.admit(transfer.bytes); err != nil {
			return err
		}
		childSource := work.child.source
		if err := legacyNativeObjectCheckpointNestedSource(ctx, childSource, work.source); err != nil {
			return err
		}
		restored, err := work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
		if restored.Object == nil {
			return ErrIntegrity
		}
		if err := legacyNativeObjectCheckpointNestedSource(ctx, restored.Source, childSource); err != nil {
			return err
		}
		work.result.Value, work.result.Parser, work.result.Scanner = restored.Object.destination, restored.Object, restored.Scanner
		work.source = restored.Source
	} else if work.state.Value != nil {
		if work.independent == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointNestedValueRestore(ctx, *work.state.Value, work.state.Model, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.independent = current
			return nil
		}
		ready, err := work.independent.step(ctx)
		if err != nil || !ready {
			return err
		}
		if err := work.admit(0); err != nil {
			return err
		}
		work.result.Value, err = work.independent.take(ctx)
		work.independent = nil
		if err != nil {
			return err
		}
	} else {
		var transfer docRangeHeaderHeapEstimate
		transfer.allocation(1, int64(work.result.Value.Type().Size()))
		if transfer.err != nil {
			return transfer.err
		}
		if err := work.admit(transfer.bytes); err != nil {
			return err
		}
	}
	return work.finish(ctx)
}

func (work *legacyNativeObjectNestedCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if !work.ready {
		if err := work.advance(ctx); err != nil {
			return work.fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectNestedCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.binding)), 1)
	heap.include(legacyNativeObjectCheckpointNestedFieldHeap(work.state))
	heap.include(work.physical)
	retained, err := work.child.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.independent != nil {
		retained, err = work.independent.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectNestedCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeObjectNestedCheckpointRestore) admit(extra int64) error {
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

func (work *legacyNativeObjectNestedCheckpointRestore) take(ctx context.Context) (legacyNativeObjectNestedRestored, error) {
	if work == nil {
		return legacyNativeObjectNestedRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectNestedRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectNestedRestored{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectNestedRestored{}, work.failure
	}
	if err := work.finish(ctx); err != nil {
		work.fail(err)
		return legacyNativeObjectNestedRestored{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectNestedRestored{}, work.failure
	}
	result := work.result
	result.Source = work.source
	work.close()
	return result, nil
}

func (work *legacyNativeObjectNestedCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectNestedCheckpointRestore) close() {
	if work != nil {
		work.child.close()
		if work.independent != nil {
			work.independent.close()
		}
		failure := work.failure
		*work = legacyNativeObjectNestedCheckpointRestore{closed: true, failure: failure}
	}
}
