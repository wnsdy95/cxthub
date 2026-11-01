package domain

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unsafe"
)

const (
	legacyNativeObjectAnyIndependent uint8 = iota
	legacyNativeObjectAnyRootMap
	legacyNativeObjectAnyResult
	legacyNativeObjectAnyVisible
)

func (state legacyNativeObjectCheckpointAnyField) validate(genericMap, initialized bool) error {
	if state.Binding > legacyNativeObjectAnyVisible ||
		state.Parser != (legacyNativeCheckpointRef{}) && !legacyNativeObjectAnyCheckpointRefValid(state.Parser) {
		return ErrIntegrity
	}
	if state.Active && state.Parser == (legacyNativeCheckpointRef{}) || state.Binding == legacyNativeObjectAnyResult && state.Active || state.Binding == legacyNativeObjectAnyVisible && !state.Active || !initialized && !state.Active && state.Binding != legacyNativeObjectAnyIndependent {
		return ErrIntegrity
	}
	if state.Binding != legacyNativeObjectAnyIndependent {
		if state.Destination != nil || state.Parser == (legacyNativeCheckpointRef{}) ||
			state.Binding == legacyNativeObjectAnyRootMap && !genericMap ||
			state.Binding == legacyNativeObjectAnyVisible && genericMap {
			return ErrIntegrity
		}
		return nil
	}
	if state.Destination == nil {
		return ErrIntegrity
	}
	value, err := legacyNativeCheckpointGraphRestoreDescriptor(context.Background(), *state.Destination)
	if err != nil {
		return err
	}
	if genericMap {
		if value.Kind != legacyNativeCheckpointObject || !value.Nil {
			return ErrIntegrity
		}
	} else if (value.Kind == legacyNativeCheckpointObject || value.Kind == legacyNativeCheckpointArray) && !value.Nil {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeObjectAnyCheckpointRefValid(reference legacyNativeCheckpointRef) bool {
	return len(reference.Hash) == len(contentHashPrefix)+64 && reference.valid()
}

func legacyNativeObjectCheckpointAnyFieldHeap(state legacyNativeObjectCheckpointAnyField) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(len(state.Parser.Hash)), 1)
	if state.Destination != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*state.Destination)))
		heap.include(legacyNativeAnyCheckpointDescriptorHeap(*state.Destination))
	}
	return heap
}

func legacyNativeObjectAnyCheckpointIndependent(value any, excess int64) (legacyNativeAnyValue, error) {
	if excess < 0 || excess%2 != 0 {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	current := legacyNativeAnyValue{value: value}
	switch decoded := value.(type) {
	case nil:
	case bool:
		current.heap.allocation(1, int64(unsafe.Sizeof(decoded)))
	case float64:
		current.heap.allocation(1, int64(unsafe.Sizeof(decoded)))
	case string:
		maximum := legacyNativeValueCheckpointPayloadLimit()
		if uint64(len(decoded)) > maximum || uint64(excess/2) > 2*maximum+64-uint64(len(decoded)) ||
			excess/2 > int64(int(^uint(0)>>1)-len(decoded)) {
			return legacyNativeAnyValue{}, ErrIntegrity
		}
		return legacyNativeAnyCheckpointString(decoded, len(decoded)+int(excess/2))
	case map[string]any:
		if decoded != nil {
			return legacyNativeAnyValue{}, fmt.Errorf("%w: legacy native any field untracked map requires initial graph owner acquisition", ErrIntegrity)
		}
		current.heap.allocation(1, int64(unsafe.Sizeof(decoded)))
	case []any:
		if decoded != nil {
			return legacyNativeAnyValue{}, fmt.Errorf("%w: legacy native any field untracked array requires initial graph owner acquisition", ErrIntegrity)
		}
		current.heap.allocation(1, int64(unsafe.Sizeof(decoded)))
	default:
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if excess != 0 {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	return current, current.validateOwnership()
}

func legacyNativeObjectAnyCheckpointSame(first, second any) bool {
	return legacyNativeAnyCheckpointSameValue(legacyNativeAnyValue{value: first}, legacyNativeAnyValue{value: second})
}

func legacyNativeObjectAnyCheckpointFieldHeap(value legacyNativeAnyValue, genericMap bool) docRangeHeaderHeapEstimate {
	heap := value.heap
	if !genericMap {
		heap.allocation(1, int64(unsafe.Sizeof(any(nil))))
	}
	return heap
}

type legacyNativeObjectAnyCheckpointSource struct {
	original LegacyByteSource
	owned    LegacyByteSource
	copied   int
	checked  int
}

func legacyNativeObjectAnyCheckpointSourceShape(current, expected LegacyByteSource) error {
	if err := legacyNativeAnyCheckpointSourceGeometry(current, expected.Bytes); err != nil {
		return err
	}
	if current.authority != expected.authority || current.PartBytes != expected.PartBytes ||
		current.partBytes() != expected.partBytes() || len(current.Parts) != len(expected.Parts) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeObjectAnyCheckpointSourcePage(ctx context.Context, current, expected LegacyByteSource, position *int) (bool, error) {
	if err := legacyNativeObjectAnyCheckpointSourceShape(current, expected); err != nil {
		return false, err
	}
	if *position < 0 || *position > len(current.Parts) {
		return false, ErrIntegrity
	}
	limit := min(len(current.Parts), *position+legacyNativeCheckpointPageItems)
	for *position < limit {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ordinal := *position
		if len(current.Parts[ordinal].Hash) != len(contentHashPrefix)+64 ||
			legacyNativeAnyCheckpointSourcePart(current, ordinal) != nil || current.Parts[ordinal] != expected.Parts[ordinal] {
			return false, ErrIntegrity
		}
		*position = ordinal + 1
	}
	return *position == len(current.Parts), ctx.Err()
}

func (source *legacyNativeObjectAnyCheckpointSource) copy(ctx context.Context) (bool, error) {
	if err := legacyNativeObjectAnyCheckpointSourceShape(source.original, source.owned); err != nil {
		return false, err
	}
	limit := min(len(source.original.Parts), source.copied+legacyNativeCheckpointPageItems)
	for source.copied < limit {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ordinal := source.copied
		if len(source.original.Parts[ordinal].Hash) != len(contentHashPrefix)+64 {
			return false, ErrIntegrity
		}
		if err := legacyNativeAnyCheckpointSourcePart(source.original, ordinal); err != nil {
			return false, err
		}
		part := source.original.Parts[ordinal]
		part.Hash = ContentHash(strings.Clone(string(part.Hash)))
		source.owned.Parts[ordinal] = part
		source.copied++
	}
	return source.copied == len(source.original.Parts), ctx.Err()
}

func (source *legacyNativeObjectAnyCheckpointSource) verify(ctx context.Context) (bool, error) {
	return legacyNativeObjectAnyCheckpointSourcePage(ctx, source.original, source.owned, &source.checked)
}

func (source *legacyNativeObjectAnyCheckpointSource) heap() docRangeHeaderHeapEstimate {
	heap := legacyNativeAnyCheckpointSourceHeap(source.original)
	heap.include(legacyNativeAnyCheckpointSourceHeap(source.owned))
	return heap
}

func legacyNativeObjectAnyCheckpointSharedSource(current, owned LegacyByteSource) int64 {
	if len(current.Parts) != len(owned.Parts) || cap(current.Parts) != cap(owned.Parts) ||
		unsafe.SliceData(current.Parts) != unsafe.SliceData(owned.Parts) {
		return 0
	}
	return legacyNativeAnyCheckpointSourceHeap(current).bytes
}

type legacyNativeObjectAnyCheckpointSnapshot struct {
	metadata     *legacyNativeObjectFields
	field        legacyNativeObjectField
	destination  uintptr
	cache        **legacyNativeAny
	cacheLength  int
	cacheCap     int
	checkpoint   *legacyJSONValueScanner
	parser       *legacyNativeAny
	value        any
	heap         docRangeHeaderHeapEstimate
	excess       int64
	active       bool
	parentActive bool
	parentField  int
}

func legacyNativeObjectAnyCheckpointSnapshotFor(native *legacyNativeObject, field int, scanner *legacyJSONValueScanner) (legacyNativeObjectAnyCheckpointSnapshot, error) {
	var snapshot legacyNativeObjectAnyCheckpointSnapshot
	if native == nil || native.metadata == nil || native.metadata.err != nil || scanner == nil || scanner.invalid ||
		!native.destination.IsValid() || native.destination.Kind() != reflect.Pointer || native.destination.IsNil() ||
		native.destination.Elem().Type() != native.metadata.model || field < 0 || field >= len(native.metadata.fields) ||
		len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) ||
		native.containers != nil && len(native.containers) != len(native.metadata.fields) {
		return snapshot, ErrIntegrity
	}
	metadata := native.metadata.fields[field]
	if metadata.genericMap == metadata.interfaceString || metadata.index < 0 || metadata.index >= native.destination.Elem().NumField() {
		return snapshot, ErrIntegrity
	}
	value := native.destination.Elem().Field(metadata.index)
	if !value.CanInterface() || metadata.genericMap && value.Type() != reflect.TypeFor[map[string]any]() ||
		metadata.interfaceString && value.Type() != reflect.TypeFor[any]() {
		return snapshot, ErrIntegrity
	}
	snapshot = legacyNativeObjectAnyCheckpointSnapshot{
		metadata: native.metadata, field: metadata, destination: native.destination.Pointer(),
		cache: unsafe.SliceData(native.containers), cacheLength: len(native.containers), cacheCap: cap(native.containers),
		checkpoint: native.checkpointScanner, value: value.Interface(), heap: native.heaps[field], excess: native.backingExcess[field],
		active: native.containerActive && native.field == field, parentActive: native.containerActive, parentField: native.field,
	}
	if native.containers != nil {
		snapshot.parser = native.containers[field]
	}
	if snapshot.heap.err != nil || snapshot.heap.bytes < 0 || snapshot.excess < 0 || snapshot.excess%2 != 0 ||
		snapshot.heap.bytes == 0 && snapshot.excess != 0 || snapshot.excess > snapshot.heap.bytes ||
		native.checkpointScanner != nil && *native.checkpointScanner != *scanner {
		return snapshot, ErrIntegrity
	}
	if snapshot.parser == nil && snapshot.active || snapshot.parser != nil && !snapshot.active && !snapshot.parser.finished {
		return snapshot, ErrIntegrity
	}
	return snapshot, nil
}

func legacyNativeObjectAnyCheckpointBinding(snapshot legacyNativeObjectAnyCheckpointSnapshot) (uint8, legacyNativeAnyValue, legacyNativeAnyValue, error) {
	parser := snapshot.parser
	if parser != nil {
		if err := legacyNativeAnyCheckpointCheckRoots(parser); err != nil {
			return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, err
		}
		if !snapshot.active && !snapshot.field.genericMap {
			if !legacyNativeObjectAnyCheckpointSame(snapshot.value, parser.result.value) || parser.visible != (legacyNativeAnyValueOwner{}) {
				return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, ErrIntegrity
			}
			return legacyNativeObjectAnyResult, legacyNativeAnyValue{}, legacyNativeAnyValue{}, parser.result.validateOwnership()
		}
		if snapshot.field.genericMap && parser.rootMap != nil {
			root, err := legacyNativeAnyCheckpointContainer(parser.rootMap, nil)
			if err != nil || !legacyNativeObjectAnyCheckpointSame(snapshot.value, root.value) {
				if err == nil {
					err = ErrIntegrity
				}
				return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, err
			}
			return legacyNativeObjectAnyRootMap, legacyNativeAnyValue{}, legacyNativeAnyValue{}, nil
		}
		if parser.visible != (legacyNativeAnyValueOwner{}) {
			visible, err := legacyNativeAnyCheckpointOwnerValue(parser.visible)
			if err != nil || snapshot.field.genericMap || !legacyNativeObjectAnyCheckpointSame(snapshot.value, visible.value) {
				if err == nil {
					err = ErrIntegrity
				}
				return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, err
			}
			return legacyNativeObjectAnyVisible, legacyNativeAnyValue{}, visible, nil
		}
	}
	independent, err := legacyNativeObjectAnyCheckpointIndependent(snapshot.value, snapshot.excess)
	if err != nil {
		return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, err
	}
	heap := legacyNativeObjectAnyCheckpointFieldHeap(independent, snapshot.field.genericMap)
	if heap.err != nil {
		return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, heap.err
	}
	if snapshot.heap.bytes != 0 && snapshot.heap != heap {
		return 0, legacyNativeAnyValue{}, legacyNativeAnyValue{}, ErrIntegrity
	}
	return legacyNativeObjectAnyIndependent, independent, legacyNativeAnyValue{}, nil
}

type legacyNativeObjectAnyCheckpoint struct {
	sharedParent *legacyNativeObjectCheckpoint
	native       *legacyNativeObject
	scanner      *legacyJSONValueScanner
	scannerShape legacyJSONValueScanner
	emptyScanner legacyJSONValueScanner
	snapshot     legacyNativeObjectAnyCheckpointSnapshot
	independent  legacyNativeAnyValue
	visible      legacyNativeAnyValue
	state        legacyNativeObjectCheckpointAnyField
	binding      ContentHash
	sources      legacyNativeObjectAnyCheckpointSource
	store        legacyNativeCheckpointIO
	child        *legacyNativeAnyCheckpoint
	graph        *legacyNativeCheckpointGraphWriter
	identity     *legacyNativeCheckpointGraphIdentity
	field        int
	budget       int64
	phase        uint8
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeObjectAnyCheckpoint(ctx context.Context, native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectAnyCheckpoint, error) {
	return newLegacyNativeObjectAnyCheckpointInObject(ctx, native, field, scanner, binding, source, store, budget, nil)
}

func newLegacyNativeObjectAnyCheckpointInObject(ctx context.Context, native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64, parent *legacyNativeObjectCheckpoint) (result *legacyNativeObjectAnyCheckpoint, failure error) {
	var registered *legacyNativeObjectAnyCheckpoint
	defer func() {
		if failure != nil && parent != nil && parent.anyTask == registered {
			parent.anyTask = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent != nil {
		if parent.closed || parent.ready || parent.anyTask != nil || parent.field != field || parent.native != native || parent.scanner != scanner || parent.state.Binding != binding || parent.state.AnyGraphNamespace != 1 || parent.anyGraph == nil || parent.anyGraph.closed {
			return nil, ErrConflict
		}
		if err := parent.checkSource(); err != nil {
			return nil, err
		}
		source, store = parent.source, parent.store
	}
	if store.Put == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return nil, err
	}
	snapshot, err := legacyNativeObjectAnyCheckpointSnapshotFor(native, field, scanner)
	if err != nil {
		return nil, err
	}
	fieldBinding, independent, visible, err := legacyNativeObjectAnyCheckpointBinding(snapshot)
	if err != nil {
		return nil, err
	}
	owned := source
	owned.Parts = nil
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectAnyCheckpoint{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	planned.allocation(int64(len(source.Parts)), 128)
	planned.allocation(int64(len(binding)), 1)
	planned.include(legacyNativeObjectAnyCheckpointFieldHeap(independent, snapshot.field.genericMap))
	if snapshot.field.genericMap && fieldBinding != legacyNativeObjectAnyIndependent {
		planned.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
	}
	planned.include(visible.heap)
	snapshot.parser.retained(&planned)
	if fieldBinding == legacyNativeObjectAnyIndependent && parent == nil {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphIdentity{})))
	}
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	owned.Parts = make([]ConversationManifestChunk, len(source.Parts))
	work := &legacyNativeObjectAnyCheckpoint{sharedParent: parent, native: native, scanner: scanner, scannerShape: *scanner, snapshot: snapshot,
		independent: independent, visible: visible, state: legacyNativeObjectCheckpointAnyField{Binding: fieldBinding, Active: snapshot.active},
		binding: ContentHash(strings.Clone(string(binding))), sources: legacyNativeObjectAnyCheckpointSource{original: source, owned: owned},
		store: store, field: field, budget: budget}
	if parent != nil {
		registered, parent.anyTask, work.identity = work, work, parent.anyGraph
	} else if fieldBinding == legacyNativeObjectAnyIndependent {
		work.identity = newLegacyNativeCheckpointGraphIdentity()
	}
	if snapshot.parser != nil {
		remaining, failure := work.remaining()
		if failure != nil {
			work.close()
			return nil, failure
		}
		var parserHeap docRangeHeaderHeapEstimate
		snapshot.parser.retained(&parserHeap)
		frame := &work.emptyScanner
		if snapshot.active {
			frame = scanner
		}
		work.child, err = newLegacyNativeAnyCheckpointInField(ctx, snapshot.parser, frame, work.binding, source.Bytes, store, remaining+parserHeap.bytes, work.sharedParentField())
		if err != nil {
			work.close()
			return nil, err
		}
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectAnyCheckpoint) checkSource() error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	if work.scanner == nil || *work.scanner != work.scannerShape {
		return ErrConflict
	}
	current, err := legacyNativeObjectAnyCheckpointSnapshotFor(work.native, work.field, work.scanner)
	if err != nil {
		return err
	}
	if current.metadata != work.snapshot.metadata || current.field != work.snapshot.field || current.destination != work.snapshot.destination ||
		current.cache != work.snapshot.cache || current.cacheLength != work.snapshot.cacheLength || current.cacheCap != work.snapshot.cacheCap ||
		current.checkpoint != work.snapshot.checkpoint || current.parser != work.snapshot.parser || current.heap != work.snapshot.heap ||
		current.excess != work.snapshot.excess || current.active != work.snapshot.active || current.parentActive != work.snapshot.parentActive ||
		current.parentField != work.snapshot.parentField || !legacyNativeObjectAnyCheckpointSame(current.value, work.snapshot.value) {
		return ErrConflict
	}
	if work.child != nil {
		if err := work.child.checkSource(); err != nil {
			return err
		}
	}
	if err := work.visible.validateOwnership(); err != nil {
		return err
	}
	return legacyNativeObjectAnyCheckpointSourceShape(work.sources.original, work.sources.owned)
}

func (work *legacyNativeObjectAnyCheckpoint) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		ready, err := work.sources.copy(ctx)
		if err != nil || !ready {
			return err
		}
		work.phase++
	case 1:
		if work.child == nil {
			work.phase++
			return nil
		}
		if err := work.limitChild(); err != nil {
			return err
		}
		if work.child.source.Read == nil {
			work.child.budget += legacyNativeAnyCheckpointSourceHeap(work.sources.owned).bytes
			return work.child.bindSource(ctx, work.sources.owned)
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Parser = work.child.root
		work.phase++
	case 2:
		if work.state.Binding != legacyNativeObjectAnyIndependent {
			work.phase++
			return nil
		}
		if work.graph == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			shared, err := work.identity.retainedBytes()
			if err != nil {
				return err
			}
			work.graph, err = newLegacyNativeCheckpointGraphWriterInField(ctx, work.independent, work.store, work.identity, remaining+shared, work.sharedParentField(), nil)
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		retained, err := work.graph.retainedBytes()
		if err != nil {
			return err
		}
		shared, err := work.identity.retainedBytes()
		if err != nil {
			return err
		}
		work.graph.budget = remaining + retained + shared
		ready, err := work.graph.step(ctx)
		if err != nil || !ready {
			return err
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointValue{})))
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		value, err := work.graph.take(ctx)
		work.graph = nil
		if err != nil {
			return err
		}
		work.state.Destination = &value
		work.phase++
	case 3:
		ready, err := work.sources.verify(ctx)
		if err != nil || !ready {
			return err
		}
		if err := work.state.validate(work.snapshot.field.genericMap, work.snapshot.heap.bytes != 0); err != nil {
			return err
		}
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeObjectAnyCheckpoint) limitChild() error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	retained, err := work.child.retainedBytes()
	if err != nil {
		return err
	}
	work.child.budget = remaining + retained
	return nil
}

func (work *legacyNativeObjectAnyCheckpoint) step(ctx context.Context) (bool, error) {
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

func (work *legacyNativeObjectAnyCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.sources.heap())
	heap.allocation(int64(len(work.binding)), 1)
	heap.include(legacyNativeObjectCheckpointAnyFieldHeap(work.state))
	heap.include(legacyNativeObjectAnyCheckpointFieldHeap(work.independent, work.snapshot.field.genericMap))
	if work.snapshot.field.genericMap && work.state.Binding != legacyNativeObjectAnyIndependent {
		heap.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
	}
	heap.include(work.visible.heap)
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		shared := legacyNativeObjectAnyCheckpointSharedSource(work.child.source, work.sources.owned)
		if retained < shared {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - shared, err: err})
	} else {
		work.snapshot.parser.retained(&heap)
	}
	if work.graph != nil {
		retained, err := work.graph.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.sharedParent == nil {
		retained, err := work.identity.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectAnyCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeObjectAnyCheckpoint) admit(extra int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
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

func (work *legacyNativeObjectAnyCheckpoint) take(ctx context.Context) (legacyNativeObjectCheckpointAnyField, error) {
	if work == nil {
		return legacyNativeObjectCheckpointAnyField{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectCheckpointAnyField{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointAnyField{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectCheckpointAnyField{}, work.failure
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointAnyField{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointAnyField{}, work.failure
	}
	if work.child != nil {
		reference, err := work.child.take(ctx)
		if err != nil || reference != work.state.Parser {
			if err == nil {
				err = ErrIntegrity
			}
			work.fail(err)
			return legacyNativeObjectCheckpointAnyField{}, work.failure
		}
	}
	result := work.state
	work.close()
	return result, nil
}

func (work *legacyNativeObjectAnyCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectAnyCheckpoint) close() {
	if work == nil || work.closed {
		return
	}
	work.child.close()
	work.graph.close()
	if work.sharedParent == nil {
		work.identity.close()
	}
	failure := work.failure
	*work = legacyNativeObjectAnyCheckpoint{closed: true, failure: failure}
}

type legacyNativeObjectAnyCheckpointRestore struct {
	sharedParent     *legacyNativeObjectCheckpointRestore
	field            int
	parserGraphBytes int64
	state            legacyNativeObjectCheckpointAnyField
	genericMap       bool
	initialized      bool
	binding          ContentHash
	sources          legacyNativeObjectAnyCheckpointSource
	store            legacyNativeCheckpointIO
	child            *legacyNativeAnyCheckpointRestore
	graph            *legacyNativeCheckpointGraphRestore
	registry         *legacyNativeCheckpointGraphRestored
	result           legacyNativeObjectAnyRestored
	value            legacyNativeAnyValue
	visible          legacyNativeAnyValue
	transferIndex    int
	budget           int64
	phase            uint8
	ready            bool
	closed           bool
	failure          error
}

func newLegacyNativeObjectAnyCheckpointRestore(ctx context.Context, saved legacyNativeObjectCheckpointAnyField, genericMap, initialized bool, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectAnyCheckpointRestore, error) {
	return newLegacyNativeObjectAnyCheckpointRestoreInObject(ctx, saved, genericMap, initialized, binding, source, store, budget, nil)
}

func newLegacyNativeObjectAnyCheckpointRestoreInObject(ctx context.Context, saved legacyNativeObjectCheckpointAnyField, genericMap, initialized bool, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64, parent *legacyNativeObjectCheckpointRestore) (result *legacyNativeObjectAnyCheckpointRestore, failure error) {
	var registered *legacyNativeObjectAnyCheckpointRestore
	defer func() {
		if failure != nil && parent != nil && parent.anyTask == registered {
			parent.anyTask = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent != nil {
		if parent.closed || parent.ready || parent.phase != 2 || parent.anyTask != nil || parent.field < 0 || parent.field >= len(parent.state.Fields) || parent.state.Fields[parent.field].Any == nil || *parent.state.Fields[parent.field].Any != saved || parent.state.AnyGraphNamespace != 1 || parent.anyGraph == nil || parent.anyGraph.closed || binding != parent.binding {
			return nil, ErrConflict
		}
		source, store = parent.source, parent.store
	}
	if store.Read == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := saved.validate(genericMap, initialized); err != nil {
		return nil, err
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectAnyCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	planned.allocation(int64(len(source.Parts)), 128)
	planned.include(legacyNativeObjectCheckpointAnyFieldHeap(saved))
	planned.allocation(int64(len(binding)), 1)
	if !genericMap {
		planned.allocation(1, int64(unsafe.Sizeof(any(nil))))
	} else if saved.Binding != legacyNativeObjectAnyIndependent {
		planned.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
	}
	if saved.Destination != nil {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestored{})))
	}
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	owned := source
	owned.Parts = make([]ConversationManifestChunk, len(source.Parts))
	saved.Parser.Hash = ContentHash(strings.Clone(string(saved.Parser.Hash)))
	if saved.Destination != nil {
		value := *saved.Destination
		value.Wire = append([]byte(nil), value.Wire...)
		value.Body.Hash = ContentHash(strings.Clone(string(value.Body.Hash)))
		value.Text.Tree.Ref.Hash = ContentHash(strings.Clone(string(value.Text.Tree.Ref.Hash)))
		saved.Destination = &value
	}
	work := &legacyNativeObjectAnyCheckpointRestore{sharedParent: parent, state: saved, genericMap: genericMap, initialized: initialized,
		binding: ContentHash(strings.Clone(string(binding))), sources: legacyNativeObjectAnyCheckpointSource{original: source, owned: owned},
		store: store, budget: budget}
	if parent != nil {
		registered, parent.anyTask, work.registry, work.field = work, work, parent.anyGraph, parent.field
	} else if saved.Destination != nil {
		work.registry = newLegacyNativeCheckpointGraphRestored()
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		ready, err := work.sources.copy(ctx)
		if err != nil || !ready {
			return err
		}
		work.phase++
	case 1:
		if work.state.Parser == (legacyNativeCheckpointRef{}) {
			work.phase = 4
			return nil
		}
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			shared := legacyNativeAnyCheckpointSourceHeap(work.sources.owned)
			work.child, err = newLegacyNativeAnyCheckpointRestoreInField(ctx, work.state.Parser, work.store, work.sources.owned, work.binding, remaining+shared.bytes, work.sharedParentField())
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		retained, err := work.child.retainedBytes()
		if err != nil {
			return err
		}
		work.child.budget = remaining + retained
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.phase++
	case 2:
		ready, err := legacyNativeObjectAnyCheckpointSourcePage(ctx, work.child.source, work.sources.owned, &work.transferIndex)
		if err != nil || !ready {
			return err
		}
		work.phase++
	case 3:
		if err := work.admit(0); err != nil {
			return err
		}
		currentSource := work.child.source
		restored, err := work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
		if err := legacyNativeObjectAnyCheckpointSourceShape(restored.Source, currentSource); err != nil {
			return err
		}
		if restored.Parser == nil {
			return ErrIntegrity
		}
		work.result.Parser, work.result.Scanner = restored.Parser, restored.Scanner
		work.parserGraphBytes = restored.GraphBytes
		work.visible = restored.Visible
		work.sources.owned = restored.Source
		work.phase++
	case 4:
		if work.state.Destination == nil {
			work.phase++
			return nil
		}
		if work.graph == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			shared, err := work.registry.retainedBytes()
			if err != nil {
				return err
			}
			work.graph, err = newLegacyNativeCheckpointGraphRestoreInField(ctx, *work.state.Destination, work.store, work.registry, remaining+shared, work.sharedParentField(), nil)
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		retained, err := work.graph.retainedBytes()
		if err != nil {
			return err
		}
		shared, err := work.registry.retainedBytes()
		if err != nil {
			return err
		}
		work.graph.budget = remaining + retained + shared
		ready, err := work.graph.step(ctx)
		if err != nil || !ready {
			return err
		}
		if err := work.admit(0); err != nil {
			return err
		}
		work.value, err = work.graph.take(ctx)
		work.graph = nil
		if err != nil {
			return err
		}
		work.phase++
	case 5:
		if err := work.finish(); err != nil {
			return err
		}
		work.phase++
	case 6:
		ready, err := work.sources.verify(ctx)
		if err != nil || !ready {
			return err
		}
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) finish() error {
	if err := work.state.validate(work.genericMap, work.initialized); err != nil {
		return err
	}
	parser := work.result.Parser
	if (parser == nil) != (work.state.Parser == (legacyNativeCheckpointRef{})) {
		return ErrIntegrity
	}
	if parser == nil {
		if work.result.Scanner != (legacyJSONValueScanner{}) || !legacyNativeAnyCheckpointSameValue(work.visible, legacyNativeAnyValue{}) {
			return ErrIntegrity
		}
	} else {
		if err := legacyNativeAnyCheckpointCheckRoots(parser); err != nil {
			return err
		}
		if work.genericMap && (parser.rootArray != nil || parser.visible != (legacyNativeAnyValueOwner{})) ||
			!work.state.Active && (!parser.finished || work.result.Scanner != (legacyJSONValueScanner{}) || parser.visible != (legacyNativeAnyValueOwner{})) {
			return ErrIntegrity
		}
	}
	switch work.state.Binding {
	case legacyNativeObjectAnyIndependent:
		if parser != nil && (!work.state.Active || parser.visible != (legacyNativeAnyValueOwner{}) || work.genericMap && parser.rootMap != nil) {
			return ErrIntegrity
		}
	case legacyNativeObjectAnyRootMap:
		if parser == nil || parser.rootMap == nil {
			return ErrIntegrity
		}
		value, err := legacyNativeAnyCheckpointContainer(parser.rootMap, nil)
		if err != nil {
			return err
		}
		work.value = value
	case legacyNativeObjectAnyResult:
		if parser == nil || !parser.finished {
			return ErrIntegrity
		}
		work.value = parser.result
	case legacyNativeObjectAnyVisible:
		if parser == nil || !work.state.Active || work.visible.owner == (legacyNativeAnyValueOwner{}) || work.visible.owner != parser.visible {
			return ErrIntegrity
		}
		work.value = work.visible
	default:
		return ErrIntegrity
	}
	if err := work.value.validateOwnership(); err != nil {
		return err
	}
	if work.genericMap {
		if _, valid := work.value.value.(map[string]any); !valid {
			return ErrIntegrity
		}
	}
	heap := legacyNativeObjectAnyCheckpointFieldHeap(work.value, work.genericMap)
	if heap.err != nil {
		return heap.err
	}
	work.result.Value = work.value.value
	work.result.GraphBytes = work.parserGraphBytes
	work.result.Heap, work.result.Excess = docRangeHeaderHeapEstimate{}, 0
	if work.initialized {
		work.result.Heap, work.result.Excess = heap, work.value.excess
	}
	if work.sharedParent != nil && work.state.Active && !work.genericMap && work.value.owner != (legacyNativeAnyValueOwner{}) && work.result.Heap.bytes != 0 {
		var storage docRangeHeaderHeapEstimate
		storage.allocation(1, int64(unsafe.Sizeof(any(nil))))
		if storage.err != nil || work.result.Heap.bytes < storage.bytes {
			return ErrIntegrity
		}
		combined := docRangeHeaderHeapEstimate{bytes: work.result.GraphBytes}
		combined.include(docRangeHeaderHeapEstimate{bytes: work.result.Heap.bytes - storage.bytes})
		if combined.err != nil {
			return combined.err
		}
		work.result.GraphBytes = combined.bytes
	}
	return nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.refreshSharedBudget(); err != nil {
		return work.fail(err)
	}
	if err := legacyNativeObjectAnyCheckpointSourceShape(work.sources.original, work.sources.owned); err != nil {
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
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.sources.heap())
	heap.allocation(int64(len(work.binding)), 1)
	heap.include(legacyNativeObjectCheckpointAnyFieldHeap(work.state))
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		shared := legacyNativeObjectAnyCheckpointSharedSource(work.child.source, work.sources.owned)
		if retained < shared {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - shared, err: err})
	} else {
		var parser docRangeHeaderHeapEstimate
		work.result.Parser.retained(&parser)
		if parser.err != nil || parser.bytes < work.parserGraphBytes {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: parser.bytes - work.parserGraphBytes})
		if work.sharedParent == nil || work.visible.owner == (legacyNativeAnyValueOwner{}) {
			heap.include(work.visible.heap)
		}
	}
	if work.state.Binding == legacyNativeObjectAnyIndependent {
		heap.include(legacyNativeObjectAnyCheckpointFieldHeap(work.value, work.genericMap))
	} else if !work.genericMap {
		heap.allocation(1, int64(unsafe.Sizeof(any(nil))))
	} else {
		heap.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
	}
	if work.graph != nil {
		retained, err := work.graph.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.sharedParent == nil {
		retained, err := work.registry.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectAnyCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) admit(extra int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
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

func (work *legacyNativeObjectAnyCheckpointRestore) take(ctx context.Context) (legacyNativeObjectAnyRestored, error) {
	if work == nil {
		return legacyNativeObjectAnyRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if err := work.finish(); err != nil {
		work.fail(err)
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if err := legacyNativeObjectAnyCheckpointSourceShape(work.sources.owned, work.sources.original); err != nil {
		work.fail(err)
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectAnyRestored{}, work.failure
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeObjectAnyRestored{}, err
	}
	result := work.result
	result.Source = work.sources.owned
	work.close()
	return result, nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectAnyCheckpointRestore) close() {
	if work == nil || work.closed {
		return
	}
	work.child.close()
	work.graph.close()
	if work.sharedParent == nil {
		work.registry.close()
	}
	failure := work.failure
	*work = legacyNativeObjectAnyCheckpointRestore{closed: true, failure: failure}
}
