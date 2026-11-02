package domain

import (
	"context"
	"math"
	"reflect"
	"unsafe"
)

type legacyNativeCheckpointGoGraphKey struct {
	address  uintptr
	length   int
	capacity int
	kind     uint8
}

func legacyNativeCheckpointGoGraphDescribe(ctx context.Context, value any) (legacyNativeCheckpointGoGraphKey, int, docRangeHeaderHeapEstimate, error) {
	var key legacyNativeCheckpointGoGraphKey
	var heap docRangeHeaderHeapEstimate
	if ctx == nil {
		return key, 0, heap, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return key, 0, heap, err
	}
	switch decoded := value.(type) {
	case nil, bool:
	case float64:
		if math.IsNaN(decoded) || math.IsInf(decoded, 0) {
			return key, 0, heap, ErrIntegrity
		}
	case string:
		if uint64(len(decoded)) > legacyNativeValueCheckpointPayloadLimit() {
			return key, 0, heap, ErrIntegrity
		}
	case map[string]any:
		if decoded != nil {
			if uint64(len(decoded)) > legacyNativeCheckpointNodes {
				return key, 0, heap, ErrIntegrity
			}
			key.address, key.kind = reflect.ValueOf(decoded).Pointer(), legacyNativeCheckpointObject
			heap = legacyNativeAnyMapBacking(len(decoded), int64(unsafe.Sizeof(any(nil))))
			heap.allocation(1, int64(unsafe.Sizeof(decoded)))
			return key, len(decoded), heap, heap.err
		}
	case []any:
		if decoded != nil {
			if uint64(cap(decoded)) > legacyNativeCheckpointNodes {
				return key, 0, heap, ErrIntegrity
			}
			key = legacyNativeCheckpointGoGraphKey{address: reflect.ValueOf(decoded).Pointer(),
				length: len(decoded), capacity: cap(decoded), kind: legacyNativeCheckpointArray}
			heap = legacyNativeAnyArrayBacking(cap(decoded))
			heap.allocation(1, int64(unsafe.Sizeof(decoded)))
			const width = uintptr(unsafe.Sizeof(any(nil)))
			if heap.err != nil || uintptr(cap(decoded)) > (^uintptr(0)-key.address)/width {
				return key, 0, heap, ErrIntegrity
			}
			return key, len(decoded), heap, heap.err
		}
	default:
		return key, 0, heap, ErrIntegrity
	}
	heap, err := legacyNativeValueHeap(ctx, reflect.ValueOf(value))
	return key, 0, heap, err
}

type legacyNativeCheckpointGoGraphSample struct {
	key      string
	source   any
	prepared legacyNativeAnyValue
	attached bool
}

type legacyNativeCheckpointGoGraphEntry struct {
	next        *legacyNativeCheckpointGoGraphEntry
	rangeLeft   *legacyNativeCheckpointGoGraphEntry
	rangeRight  *legacyNativeCheckpointGoGraphEntry
	rangeParent *legacyNativeCheckpointGoGraphEntry
	rangeHeight int
	key         legacyNativeCheckpointGoGraphKey
	source      any
	length      int
	object      *legacyNativeAnyMap
	array       *legacyNativeAnyArray
	value       legacyNativeAnyValue
	samples     legacyCanonicalPages[legacyNativeCheckpointGoGraphSample]
	sourceBytes int64
	nativeBytes int64
	descendants int64
	owned       int64
	complete    bool
	fullBacking bool
	verified    bool
	synthetic   bool
}

type legacyNativeCheckpointGoGraphRegistry struct {
	source         *legacyNativeCheckpointGoGraphSet
	root           legacyNativeCheckpointGoGraphLink
	ranges         *legacyNativeCheckpointGoGraphEntry
	first          *legacyNativeCheckpointGoGraphEntry
	last           *legacyNativeCheckpointGoGraphEntry
	active         *legacyNativeCheckpointGoGraph
	capture        *legacyNormalizationCapture
	backingCapture *legacyNormalizationCapture
	layout         *legacyNativeCheckpointGoLayout
	nodes          int64
	entries        int64
	owned          int64
	sourceBytes    int64
	closed         bool
	failure        error
}

func newLegacyNativeCheckpointGoGraphRegistry() *legacyNativeCheckpointGoGraphRegistry {
	return &legacyNativeCheckpointGoGraphRegistry{}
}

func (registry *legacyNativeCheckpointGoGraphRegistry) metadataBytes(nodes, entries int64) (int64, error) {
	if nodes < 0 || entries < 0 || uint64(entries) > legacyNativeCheckpointNodes ||
		nodes > max(entries-1, 0) {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*registry)))
	legacyCanonicalStorageAllocation(&heap, int(nodes), int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphLookup{})))
	legacyCanonicalStorageAllocation(&heap, int(entries), int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphEntry{})))
	return heap.bytes, heap.err
}

func (registry *legacyNativeCheckpointGoGraphRegistry) retainedBytes() (int64, error) {
	if registry == nil || registry.closed {
		return 0, nil
	}
	if registry.owned < 0 || registry.root.empty() != (registry.entries == 0) ||
		registry.root.branch != nil && registry.root.entry != nil ||
		(registry.first == nil) != (registry.entries == 0) || (registry.last == nil) != (registry.entries == 0) {
		return 0, ErrIntegrity
	}
	metadata, err := registry.metadataBytes(registry.nodes, registry.entries)
	heap := docRangeHeaderHeapEstimate{bytes: metadata, err: err}
	heap.include(docRangeHeaderHeapEstimate{bytes: registry.owned})
	if registry.backingCapture != nil {
		heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
		heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	}
	if registry.source != nil && registry.source.record != nil {
		if err := registry.source.checkOwnerScope(); err != nil {
			return 0, err
		}
		if registry.sourceBytes < 0 || registry.sourceBytes > registry.owned || registry.sourceBytes > registry.source.record.record.heap.bytes {
			return 0, ErrIntegrity
		}
		heap.bytes -= registry.sourceBytes
	}
	return heap.bytes, heap.err
}

func (registry *legacyNativeCheckpointGoGraphRegistry) close() {
	if registry != nil {
		if registry.backingCapture != nil {
			registry.backingCapture.release()
		}
		failure := registry.failure
		*registry = legacyNativeCheckpointGoGraphRegistry{closed: true, failure: failure}
	}
}

func (registry *legacyNativeCheckpointGoGraphRegistry) fail(err error) error {
	if registry != nil {
		if registry.failure != nil {
			return registry.failure
		}
		registry.close()
		registry.failure = err
	}
	return err
}

func (entry *legacyNativeCheckpointGoGraphEntry) validateSource(ctx context.Context) error {
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, entry.source)
	if err != nil {
		return err
	}
	if key != entry.key || length != entry.length {
		return ErrConflict
	}
	return nil
}

func (entry *legacyNativeCheckpointGoGraphEntry) physicalLength() int {
	if entry.array != nil && entry.fullBacking {
		return entry.key.capacity
	}
	return entry.length
}

func (entry *legacyNativeCheckpointGoGraphEntry) nativeHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if entry.object != nil && entry.array == nil {
		heap = entry.object.heap()
		entry.object.retained(&heap)
	} else if entry.array != nil && entry.object == nil {
		heap = entry.array.heap()
		entry.array.retained(&heap)
	} else {
		heap.err = ErrIntegrity
	}
	return heap
}

func (registry *legacyNativeCheckpointGoGraphRegistry) account(entry *legacyNativeCheckpointGoGraphEntry) error {
	native := entry.nativeHeap()
	if native.err != nil {
		return native.err
	}
	if entry.sourceBytes < 0 || entry.descendants < 0 || entry.descendants > native.bytes ||
		entry.owned < 0 || entry.owned > registry.owned || !entry.samples.valid() {
		return ErrIntegrity
	}
	heap := docRangeHeaderHeapEstimate{bytes: native.bytes - entry.descendants}
	if entry.array != nil && entry.array.resource != nil {
		heap = entry.array.viewMetadataHeap()
	}
	heap.include(docRangeHeaderHeapEstimate{bytes: entry.sourceBytes})
	entry.samples.retained(&heap)
	if heap.err != nil {
		return heap.err
	}
	next := docRangeHeaderHeapEstimate{bytes: registry.owned - entry.owned}
	next.include(heap)
	if next.err != nil {
		return next.err
	}
	registry.owned, entry.owned, entry.nativeBytes = next.bytes, heap.bytes, native.bytes-entry.descendants
	if entry.array != nil && entry.array.resource != nil {
		entry.nativeBytes = entry.array.viewMetadataHeap().bytes
	}
	return nil
}

const (
	legacyNativeCheckpointGoGraphStart uint8 = iota
	legacyNativeCheckpointGoGraphOverlap
	legacyNativeCheckpointGoGraphTail
	legacyNativeCheckpointGoGraphBalance
	legacyNativeCheckpointGoGraphIndex
	legacyNativeCheckpointGoGraphNext
	legacyNativeCheckpointGoGraphPush
	legacyNativeCheckpointGoGraphWaiting
	legacyNativeCheckpointGoGraphAttach
	legacyNativeCheckpointGoGraphFinish
	legacyNativeCheckpointGoGraphWaitingBacking
	legacyNativeCheckpointGoGraphPrepareBacking
)

type legacyNativeCheckpointGoGraphFrame struct {
	parent      *legacyNativeCheckpointGoGraphFrame
	entry       *legacyNativeCheckpointGoGraphEntry
	overlap     *legacyNativeCheckpointGoGraphEntry
	rangeParent *legacyNativeCheckpointGoGraphEntry
	source      any
	key         legacyNativeCheckpointGoGraphKey
	length      int
	sourceHeap  docRangeHeaderHeapEstimate
	iterator    *reflect.MapIter
	indexNode   *legacyNativeCheckpointGoGraphLink
	indexLevel  int
	indexLimit  int
	child       legacyNativeAnyValue
	position    int
	charge      int64
	phase       uint8
	fullBacking bool
	synthetic   bool
	group       *legacyNativeGoLayoutGroup
}

func legacyNativeCheckpointGoGraphFrameHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphFrame{})))
	heap.allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
	heap.allocation(256, 1)
	return heap
}

func legacyNativeCheckpointGoGraphPageRetirement[Value any](pages *legacyCanonicalPages[Value], future docRangeHeaderHeapEstimate) docRangeHeaderHeapEstimate {
	var current docRangeHeaderHeapEstimate
	pages.retained(&current)
	if current.err != nil {
		return current
	}
	if future.err != nil {
		return future
	}
	var retired docRangeHeaderHeapEstimate
	if current.bytes != future.bytes {
		if pages.single != nil {
			retired.allocation(1, int64(unsafe.Sizeof(*pages.single)))
		} else if pages.inline != nil {
			retired.allocation(1, int64(unsafe.Sizeof(*pages.inline)))
		}
	}
	return retired
}

type legacyNativeCheckpointGoGraph struct {
	registry       *legacyNativeCheckpointGoGraphRegistry
	frame          *legacyNativeCheckpointGoGraphFrame
	root           *legacyNativeCheckpointGoGraphEntry
	verify         *legacyNativeCheckpointGoGraphEntry
	result         legacyNativeAnyValue
	frameBytes     int64
	resultBytes    int64
	budget         int64
	depth          int
	verifyPosition int
	verifying      bool
	ready          bool
	closed         bool
	failure        error
	layout         *legacyNativeCheckpointGoLayout
	resource       *legacyNativeResourcePreparation
}

func newLegacyNativeCheckpointGoGraph(ctx context.Context, value any, registry *legacyNativeCheckpointGoGraphRegistry, budget int64) (*legacyNativeCheckpointGoGraph, error) {
	if registry != nil && registry.failure != nil {
		return nil, registry.failure
	}
	if ctx == nil {
		return nil, registry.fail(ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return nil, registry.fail(err)
	}
	if registry == nil || registry.closed || budget <= 0 {
		return nil, registry.fail(ErrIntegrity)
	}
	if registry.active != nil {
		return nil, registry.fail(ErrConflict)
	}
	if registry.source != nil {
		if err := registry.source.checkSource(); err != nil {
			return nil, registry.fail(err)
		}
		if registry.source.registry != registry || registry.source.prepare != nil || registry.source.phase != 0 || registry.source.position < 0 || registry.source.position >= len(registry.source.values) ||
			!legacyNativeCheckpointGoGraphSetSame(value, registry.source.values[registry.source.position]) {
			return nil, registry.fail(ErrConflict)
		}
	}
	key, length, sourceHeap, err := legacyNativeCheckpointGoGraphDescribe(ctx, value)
	if err != nil {
		return nil, registry.fail(err)
	}
	frameHeap := legacyNativeCheckpointGoGraphFrameHeap()
	frameHeap.include(sourceHeap)
	planned := frameHeap
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraph{})))
	shared, err := registry.retainedBytes()
	planned.include(docRangeHeaderHeapEstimate{bytes: shared, err: err})
	if planned.err != nil {
		return nil, registry.fail(planned.err)
	}
	if planned.bytes > budget {
		return nil, registry.fail(ErrDocumentResourceLimit)
	}
	if registry.source != nil {
		if err := registry.source.admit(planned.bytes - shared); err != nil {
			return nil, registry.fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, registry.fail(err)
	}
	work := &legacyNativeCheckpointGoGraph{registry: registry, budget: budget, depth: 1, frameBytes: frameHeap.bytes}
	work.frame = &legacyNativeCheckpointGoGraphFrame{source: value, key: key, length: length,
		sourceHeap: sourceHeap, charge: frameHeap.bytes}
	registry.active = work
	return work, nil
}

func (work *legacyNativeCheckpointGoGraph) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	if work.frameBytes < 0 || work.resultBytes < 0 {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(docRangeHeaderHeapEstimate{bytes: work.frameBytes})
	heap.include(docRangeHeaderHeapEstimate{bytes: work.resultBytes})
	for _, owned := range []interface{ retainedBytes() (int64, error) }{work.layout, work.resource} {
		bytes, err := owned.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointGoGraph) admit(additional, released int64) error {
	if work.registry != nil && work.registry.failure != nil {
		return work.registry.failure
	}
	if work.registry == nil || work.registry.closed || additional < 0 || released < 0 {
		return ErrIntegrity
	}
	if capture := work.registry.capture; capture != nil && !capture.valid(capture.scope) {
		return ErrConflict
	}
	current, err := work.retainedBytes()
	if err != nil {
		return err
	}
	shared, err := work.registry.retainedBytes()
	heap := docRangeHeaderHeapEstimate{bytes: current}
	heap.include(docRangeHeaderHeapEstimate{bytes: shared, err: err})
	if heap.err != nil {
		return heap.err
	}
	if released > heap.bytes {
		return ErrIntegrity
	}
	heap.bytes -= released
	heap.include(docRangeHeaderHeapEstimate{bytes: additional})
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	if work.registry.source != nil {
		if work.registry.source.registry != work.registry || work.registry.source.prepare != work {
			return ErrConflict
		}
		remaining, err := work.registry.source.remaining()
		if err != nil {
			return err
		}
		if released > math.MaxInt64-remaining {
			return ErrIntegrity
		}
		if additional > remaining+released {
			return ErrDocumentResourceLimit
		}
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraph) push(ctx context.Context, source any) error {
	if work.depth >= legacyNativeCheckpointGraphCaptureDepth {
		return ErrIntegrity
	}
	key, length, sourceHeap, err := legacyNativeCheckpointGoGraphDescribe(ctx, source)
	if err != nil {
		return err
	}
	heap := legacyNativeCheckpointGoGraphFrameHeap()
	heap.include(sourceHeap)
	if heap.err != nil {
		return heap.err
	}
	if err := work.admit(heap.bytes, 0); err != nil {
		return err
	}
	work.frame = &legacyNativeCheckpointGoGraphFrame{parent: work.frame, source: source, key: key,
		length: length, sourceHeap: sourceHeap, charge: heap.bytes}
	work.frameBytes += heap.bytes
	work.depth++
	return nil
}

func (work *legacyNativeCheckpointGoGraph) begin(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, frame.source)
	if err != nil {
		return err
	}
	if key != frame.key || length != frame.length {
		return ErrConflict
	}
	if key.kind == 0 {
		value := legacyNativeAnyValue{value: frame.source, heap: frame.sourceHeap}
		if text, ok := frame.source.(string); ok {
			value.capacity = len(text)
		}
		if err := value.validateOwnership(); err != nil {
			return err
		}
		return work.complete(frame, value)
	}
	entry, err := work.registry.lookup(key)
	if err != nil {
		return err
	}
	if entry != nil && entry.key == key {
		if !entry.complete {
			return ErrIntegrity
		}
		if err := entry.validateSource(ctx); err != nil {
			return err
		}
		if err := entry.value.validateOwnership(); err != nil {
			return err
		}
		if frame.parent == nil {
			work.root = entry
		}
		return work.complete(frame, entry.value)
	}
	if key.kind == legacyNativeCheckpointArray {
		layout := work.registry.layout
		if layout == nil {
			layout = work.layout
		}
		if layout != nil && layout.ready && !frame.synthetic {
			group, err := layout.backing(key)
			if err != nil {
				return err
			}
			if group != nil {
				frame.group = group
				if group.owner == nil {
					if group.preparing {
						return ErrIntegrity
					}
					group.preparing = true
					frame.phase = legacyNativeCheckpointGoGraphWaitingBacking
					if err := work.push(ctx, group.source); err != nil {
						return err
					}
					work.frame.synthetic = true
					return nil
				}
				frame.position, frame.phase = frame.length, legacyNativeCheckpointGoGraphTail
				return nil
			}
		}
		frame.overlap, frame.phase = work.registry.ranges, legacyNativeCheckpointGoGraphOverlap
		return nil
	}
	return work.register(ctx, frame)
}

func (work *legacyNativeCheckpointGoGraph) checkOverlap(frame *legacyNativeCheckpointGoGraphFrame) error {
	const width = uintptr(unsafe.Sizeof(any(nil)))
	end := frame.key.address + uintptr(frame.key.capacity)*width
	for checked := 0; checked < legacyNativeCheckpointPageItems && frame.overlap != nil; checked++ {
		other := frame.overlap
		if frame.key.capacity == 0 {
			frame.overlap = nil
			break
		}
		otherEnd := other.key.address + uintptr(other.key.capacity)*width
		frame.rangeParent = other
		if end <= other.key.address {
			frame.overlap = other.rangeLeft
		} else if frame.key.address >= otherEnd {
			frame.overlap = other.rangeRight
		} else {
			return ErrIntegrity
		}
	}
	if frame.overlap == nil {
		frame.position, frame.phase = frame.length, legacyNativeCheckpointGoGraphTail
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraph) checkTail(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, frame.source)
	if err != nil {
		return err
	}
	if key != frame.key || length != frame.length {
		return ErrConflict
	}
	backing := frame.source.([]any)[:frame.key.capacity]
	for checked := 0; checked < legacyNativeCheckpointPageItems && frame.position < len(backing); checked++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if backing[frame.position] != nil {
			frame.fullBacking = true
			frame.position = len(backing)
			break
		}
		frame.position++
	}
	if frame.position < len(backing) {
		return nil
	}
	return work.register(ctx, frame)
}

func (work *legacyNativeCheckpointGoGraph) register(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	registry := work.registry
	entry, err := registry.lookup(frame.key)
	if err != nil {
		return err
	}
	if entry != nil && entry.key == frame.key || uint64(registry.entries) >= legacyNativeCheckpointNodes {
		return ErrIntegrity
	}
	missing, split := int64(0), -1
	if entry != nil {
		missing, split = 1, frame.key.difference(entry.key)
	}
	var native docRangeHeaderHeapEstimate
	if frame.key.kind == legacyNativeCheckpointObject {
		native = legacyNativeAnyMapBacking(0, int64(unsafe.Sizeof(any(nil))))
		native.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
		native.include(legacyNativeAnyMapMetadata(0, docRangeHeaderHeapEstimate{}))
	} else {
		native = legacyNativeAnyArrayBacking(frame.key.capacity)
		native.allocation(1, int64(unsafe.Sizeof([]any(nil))))
		native.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyArray{})))
	}
	before, err := registry.metadataBytes(registry.nodes, registry.entries)
	if err != nil {
		return err
	}
	after, err := registry.metadataBytes(registry.nodes+missing, registry.entries+1)
	if err != nil || after < before {
		return ErrIntegrity
	}
	planned := native
	planned.include(frame.sourceHeap)
	planned.include(docRangeHeaderHeapEstimate{bytes: after - before})
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes, frame.sourceHeap.bytes); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry = &legacyNativeCheckpointGoGraphEntry{key: frame.key, source: frame.source,
		length: frame.length, sourceBytes: frame.sourceHeap.bytes, fullBacking: frame.fullBacking, synthetic: frame.synthetic}
	if frame.key.kind == legacyNativeCheckpointObject {
		entry.object = newLegacyNativeAnyMap()
	} else {
		if frame.group != nil {
			offset := int((frame.key.address - frame.group.start) / uintptr(unsafe.Sizeof(any(nil))))
			entry.array, err = newLegacyNativeResourceArray(ctx, frame.group.owner, offset, frame.length, frame.key.capacity)
		} else {
			entry.array, err = newLegacyNativeAnyArray(frame.key.capacity)
		}
		if err != nil {
			return err
		}
		if frame.group == nil {
			entry.array.decoded = make([]any, 0, frame.key.capacity)
			entry.array.tracked = true
			entry.array.estimate.include(legacyNativeAnyArrayBacking(frame.key.capacity))
		}
		if frame.key.capacity > 0 && frame.group == nil {
			if err := registry.insertRange(entry, frame.rangeParent); err != nil {
				return err
			}
		}
	}
	if registry.root.empty() {
		registry.root.entry = entry
	}
	if registry.last == nil {
		registry.first = entry
	} else {
		registry.last.next = entry
	}
	registry.last = entry
	registry.entries++
	work.frameBytes -= frame.sourceHeap.bytes
	frame.charge -= frame.sourceHeap.bytes
	frame.source = nil
	frame.entry, frame.position, frame.phase = entry, 0, legacyNativeCheckpointGoGraphIndex
	if entry.rangeParent != nil {
		frame.rangeParent, frame.phase = entry.rangeParent, legacyNativeCheckpointGoGraphBalance
	}
	frame.indexNode, frame.indexLevel, frame.indexLimit = &registry.root, split, legacyNativeCheckpointGoGraphKeyBits
	if frame.parent == nil {
		work.root = entry
	}
	if !entry.synthetic {
		registry.sourceBytes += frame.sourceHeap.bytes
	} else {
		layout := work.registry.layout
		if layout == nil {
			layout = work.layout
		}
		layout.allocations -= legacyNativeAnyArrayBacking(frame.key.capacity).bytes
	}
	return registry.account(entry)
}

func (work *legacyNativeCheckpointGoGraph) next(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	entry := frame.entry
	if err := entry.validateSource(ctx); err != nil {
		return err
	}
	var key string
	var source any
	if entry.object != nil {
		if !frame.iterator.Next() {
			if frame.position != entry.length || entry.samples.length() != entry.length {
				return ErrConflict
			}
			frame.iterator, frame.phase = nil, legacyNativeCheckpointGoGraphFinish
			return nil
		}
		if frame.position >= entry.length {
			return ErrConflict
		}
		key, source = frame.iterator.Key().String(), frame.iterator.Value().Interface()
	} else {
		if frame.position == entry.physicalLength() {
			frame.phase = legacyNativeCheckpointGoGraphFinish
			return nil
		}
		source = entry.source.([]any)[:entry.key.capacity][frame.position]
	}
	childKey, _, sourceHeap, err := legacyNativeCheckpointGoGraphDescribe(ctx, source)
	if err != nil {
		return err
	}
	var sourceIncrement docRangeHeaderHeapEstimate
	if entry.object != nil {
		if uint64(len(key)) > legacyNativeValueCheckpointPayloadLimit() {
			return ErrIntegrity
		}
		sourceIncrement.allocation(int64(len(key)), 1)
	}
	if childKey.kind == 0 {
		sourceIncrement.include(sourceHeap)
	}
	storage, future, err := entry.samples.appendRequirements()
	if err != nil {
		return err
	}
	if storage > canonicalCheckBytes {
		return ErrIntegrity
	}
	var current docRangeHeaderHeapEstimate
	entry.samples.retained(&current)
	if current.err != nil || future.err != nil || future.bytes < current.bytes {
		return ErrIntegrity
	}
	planned := sourceIncrement
	planned.include(docRangeHeaderHeapEstimate{bytes: future.bytes - current.bytes})
	planned.include(legacyNativeCheckpointGoGraphPageRetirement(&entry.samples, future))
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes, 0); err != nil {
		return err
	}
	if err := entry.samples.append(legacyNativeCheckpointGoGraphSample{key: key, source: source}); err != nil {
		return err
	}
	entry.sourceBytes += sourceIncrement.bytes
	if !entry.synthetic {
		work.registry.sourceBytes += sourceIncrement.bytes
	}
	frame.phase = legacyNativeCheckpointGoGraphPush
	return work.registry.account(entry)
}

func (work *legacyNativeCheckpointGoGraph) attach(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	entry := frame.entry
	if err := entry.validateSource(ctx); err != nil {
		return err
	}
	sample := entry.samples.at(frame.position)
	if sample == nil || sample.attached {
		return ErrIntegrity
	}
	descendants := docRangeHeaderHeapEstimate{bytes: entry.descendants}
	if frame.child.owner.object != nil || frame.child.owner.array != nil {
		descendants.include(frame.child.heap)
		descendants.include(docRangeHeaderHeapEstimate{bytes: frame.child.owner.bytes})
	}
	if descendants.err != nil {
		return descendants.err
	}
	var retirement docRangeHeaderHeapEstimate
	if entry.object != nil {
		_, future, err := entry.object.links.appendRequirements()
		if err != nil {
			return err
		}
		retirement = legacyNativeCheckpointGoGraphPageRetirement(&entry.object.links, future)
	} else {
		pages := entry.array.children
		if frame.position >= entry.length {
			pages = legacyCanonicalPages[legacyNativeAnyArrayChild]{}
			if entry.array.tail != nil {
				pages = entry.array.tail.children
			}
		}
		_, future, err := pages.appendRequirements()
		if err != nil {
			return err
		}
		if frame.position < entry.length {
			retirement = legacyNativeCheckpointGoGraphPageRetirement(&pages, future)
		}
	}
	if retirement.err != nil {
		return retirement.err
	}
	guard := func(future docRangeHeaderHeapEstimate) error {
		if future.err != nil {
			return future.err
		}
		if future.bytes < descendants.bytes {
			return ErrIntegrity
		}
		planned := docRangeHeaderHeapEstimate{bytes: future.bytes - descendants.bytes}
		planned.include(retirement)
		if planned.err != nil {
			return planned.err
		}
		return work.admit(planned.bytes, entry.nativeBytes)
	}
	if entry.object != nil {
		if _, found := entry.object.decoded[sample.key]; found {
			return ErrConflict
		}
		cost, err := entry.object.putBytes(sample.key)
		if err != nil {
			return err
		}
		if cost > canonicalCheckBytes {
			return ErrIntegrity
		}
		if _, err := entry.object.putValueGuarded(sample.key, len(sample.key), frame.child, guard); err != nil {
			return err
		}
	} else {
		input := &legacyNormalizationInput{}
		var appended bool
		var err error
		if frame.position < entry.length {
			appended, err = entry.array.appendValueGuarded(ctx, input, frame.child, guard)
		} else {
			appended, err = entry.array.appendTailValueGuarded(ctx, input, frame.child, guard)
		}
		if err != nil {
			return err
		}
		if !appended || input.workBytes > canonicalCheckBytes || entry.array.previous != nil || cap(entry.array.decoded) != entry.key.capacity {
			return ErrIntegrity
		}
	}
	entry.descendants = descendants.bytes
	sample.prepared, sample.attached = frame.child, true
	frame.child = legacyNativeAnyValue{}
	frame.position++
	frame.phase = legacyNativeCheckpointGoGraphNext
	return work.registry.account(entry)
}

func (work *legacyNativeCheckpointGoGraph) finish(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	entry := frame.entry
	if err := entry.validateSource(ctx); err != nil {
		return err
	}
	if frame.position != entry.physicalLength() || entry.samples.length() != entry.physicalLength() {
		return ErrIntegrity
	}
	var metadata docRangeHeaderHeapEstimate
	var value legacyNativeAnyValue
	if entry.object != nil {
		if !entry.object.valid() || len(entry.object.decoded) != entry.length {
			return ErrIntegrity
		}
		entry.object.retained(&metadata)
		value = legacyNativeAnyValue{value: entry.object.values(), heap: entry.object.heap(), excess: entry.object.backingExcess(),
			owner: legacyNativeAnyValueOwner{object: entry.object, revision: entry.object.revision, bytes: metadata.bytes}}
	} else {
		if len(entry.array.decoded) != entry.length || cap(entry.array.decoded) != entry.key.capacity ||
			!entry.array.tracked || !entry.array.children.valid() || entry.array.children.length() != entry.length {
			return ErrIntegrity
		}
		if entry.fullBacking {
			if err := entry.array.validateTail(true); err != nil {
				return err
			}
		} else if entry.array.tail != nil {
			return ErrIntegrity
		}
		entry.array.retained(&metadata)
		if entry.array.resource != nil {
			entry.array.finishResource()
		}
		value = legacyNativeAnyValue{value: entry.array.values(), heap: entry.array.heap(), excess: entry.array.backingExcess(),
			owner: legacyNativeAnyValueOwner{array: entry.array, revision: entry.array.revision, bytes: metadata.bytes}}
	}
	if metadata.err != nil {
		return metadata.err
	}
	if err := value.validateOwnership(); err != nil {
		return err
	}
	entry.value, entry.complete = value, true
	return work.complete(frame, value)
}

func (work *legacyNativeCheckpointGoGraph) complete(frame *legacyNativeCheckpointGoGraphFrame, value legacyNativeAnyValue) error {
	if work.frame != frame || work.depth <= 0 || frame.charge < 0 || frame.charge > work.frameBytes {
		return ErrIntegrity
	}
	var resultBytes int64
	if frame.parent == nil && value.owner.object == nil && value.owner.array == nil {
		resultBytes = value.heap.bytes
	}
	if err := work.admit(resultBytes, frame.charge); err != nil {
		return err
	}
	work.frameBytes -= frame.charge
	work.frame, work.depth = frame.parent, work.depth-1
	if work.frame != nil {
		if work.frame.phase == legacyNativeCheckpointGoGraphWaitingBacking {
			work.frame.group.entry = frame.entry
			work.frame.child, work.frame.phase = value, legacyNativeCheckpointGoGraphPrepareBacking
			return nil
		}
		if work.frame.phase != legacyNativeCheckpointGoGraphWaiting {
			return ErrIntegrity
		}
		work.frame.child, work.frame.phase = value, legacyNativeCheckpointGoGraphAttach
	} else {
		work.result, work.resultBytes = value, resultBytes
		work.verify, work.verifyPosition, work.verifying = work.registry.first, 0, true
	}
	return nil
}

func legacyNativeCheckpointGoGraphSame(first, second any) bool {
	switch decoded := first.(type) {
	case nil:
		return second == nil
	case bool:
		other, ok := second.(bool)
		return ok && decoded == other
	case float64:
		other, ok := second.(float64)
		return ok && math.Float64bits(decoded) == math.Float64bits(other)
	case string:
		other, ok := second.(string)
		return ok && len(decoded) == len(other) && (len(decoded) == 0 || unsafe.StringData(decoded) == unsafe.StringData(other))
	case map[string]any:
		other, ok := second.(map[string]any)
		return ok && (decoded == nil) == (other == nil) && reflect.ValueOf(decoded).Pointer() == reflect.ValueOf(other).Pointer()
	case []any:
		other, ok := second.([]any)
		return ok && (decoded == nil) == (other == nil) && len(decoded) == len(other) && cap(decoded) == cap(other) &&
			reflect.ValueOf(decoded).Pointer() == reflect.ValueOf(other).Pointer()
	default:
		return false
	}
}

func (work *legacyNativeCheckpointGoGraph) verifySources(ctx context.Context) error {
	entry := work.verify
	if entry == nil {
		if err := work.validateResult(ctx); err != nil {
			return err
		}
		work.verifying, work.ready = false, true
		return nil
	}
	if !entry.complete || !entry.samples.valid() || !entry.verified && entry.samples.length() != entry.physicalLength() {
		return ErrIntegrity
	}
	if err := entry.validateSource(ctx); err != nil {
		return err
	}
	if err := entry.value.validateOwnership(); err != nil {
		return err
	}
	if entry.verified {
		capture := work.registry.capture
		if capture == nil || !capture.valid(capture.scope) {
			return ErrConflict
		}
		if entry.samples.length() != 0 || work.verifyPosition != 0 {
			return ErrIntegrity
		}
		work.verify = entry.next
		return nil
	}
	limit := entry.length
	if entry.array != nil {
		limit = entry.key.capacity
		if len(entry.array.decoded) != entry.length || cap(entry.array.decoded) != entry.key.capacity {
			return ErrConflict
		}
	} else if len(entry.object.decoded) != entry.length {
		return ErrConflict
	}
	for checked := 0; checked < legacyNativeCheckpointPageItems && work.verifyPosition < limit; checked++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		position := work.verifyPosition
		if entry.array != nil && !entry.fullBacking && position >= entry.length {
			if entry.source.([]any)[:limit][position] != nil || entry.array.decoded[:limit][position] != nil {
				return ErrConflict
			}
			work.verifyPosition++
			continue
		}
		{
			sample := entry.samples.at(position)
			if sample == nil || !sample.attached {
				return ErrIntegrity
			}
			var source, prepared any
			if entry.object != nil {
				var found bool
				source, found = entry.source.(map[string]any)[sample.key]
				if !found {
					return ErrConflict
				}
				prepared, found = entry.object.decoded[sample.key]
				if !found {
					return ErrConflict
				}
			} else {
				source, prepared = entry.source.([]any)[:limit][position], entry.array.decoded[:limit][position]
			}
			if !legacyNativeCheckpointGoGraphSame(source, sample.source) || !legacyNativeCheckpointGoGraphSame(prepared, sample.prepared.value) {
				return ErrConflict
			}
			if err := sample.prepared.validateOwnership(); err != nil {
				return err
			}
		}
		work.verifyPosition++
	}
	if work.verifyPosition == limit {
		if capture := work.registry.capture; capture != nil {
			if !capture.valid(capture.scope) {
				return ErrConflict
			}
			entry.samples = legacyCanonicalPages[legacyNativeCheckpointGoGraphSample]{}
			entry.verified = true
			if err := work.registry.account(entry); err != nil {
				return err
			}
		}
		work.verify, work.verifyPosition = entry.next, 0
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraph) validateResult(ctx context.Context) error {
	if work.registry.source != nil {
		if err := work.admit(0, 0); err != nil {
			return err
		}
	}
	if work.frame != nil || work.depth != 0 || work.frameBytes != 0 {
		return ErrIntegrity
	}
	if work.root != nil {
		if !work.root.complete {
			return ErrIntegrity
		}
		if err := work.root.validateSource(ctx); err != nil {
			return err
		}
	}
	return work.result.validateOwnership()
}

func (work *legacyNativeCheckpointGoGraph) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if ctx == nil {
		return work.fail(ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.registry == nil || work.registry.active != work {
		if work.registry != nil && work.registry.failure != nil {
			return work.fail(work.registry.failure)
		}
		return work.fail(ErrIntegrity)
	}
	if err := work.admit(0, 0); err != nil {
		return work.fail(err)
	}
	if work.registry.layout == nil && work.frame != nil && work.frame.phase == legacyNativeCheckpointGoGraphStart && work.layout == nil && work.frame.key.kind != 0 {
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoLayout{})))
		planned.allocation(1, int64(unsafe.Sizeof(any(nil))))
		if planned.err != nil {
			return work.fail(planned.err)
		}
		if err := work.admit(planned.bytes, 0); err != nil {
			return work.fail(err)
		}
		work.layout = &legacyNativeCheckpointGoLayout{work: work, roots: []any{work.frame.source}}
	}
	if work.layout != nil && !work.layout.ready {
		if err := work.layout.step(ctx); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	for actions := 0; actions < 8; actions++ {
		ready, pause, err := work.advanceBounded(ctx)
		if err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.admit(0, 0); err != nil {
			return work.fail(err)
		}
		if ready || pause {
			return ready, nil
		}
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointGoGraph) advanceBounded(ctx context.Context) (bool, bool, error) {
	var err error
	pause := work.ready || work.verifying
	if work.ready {
		err = work.validateResult(ctx)
	} else if work.verifying {
		err = work.verifySources(ctx)
	} else {
		frame := work.frame
		if frame == nil {
			return false, false, ErrIntegrity
		}
		pause = frame.phase == legacyNativeCheckpointGoGraphAttach || frame.phase == legacyNativeCheckpointGoGraphOverlap || frame.phase == legacyNativeCheckpointGoGraphTail || frame.phase == legacyNativeCheckpointGoGraphIndex
		switch frame.phase {
		case legacyNativeCheckpointGoGraphStart:
			err = work.begin(ctx, frame)
		case legacyNativeCheckpointGoGraphOverlap:
			err = work.checkOverlap(frame)
		case legacyNativeCheckpointGoGraphTail:
			err = work.checkTail(ctx, frame)
		case legacyNativeCheckpointGoGraphBalance:
			err = work.balanceRanges(frame)
		case legacyNativeCheckpointGoGraphIndex:
			err = work.index(ctx, frame)
		case legacyNativeCheckpointGoGraphNext:
			err = work.next(ctx, frame)
		case legacyNativeCheckpointGoGraphPush:
			sample := frame.entry.samples.at(frame.position)
			if sample == nil || sample.attached {
				err = ErrIntegrity
			} else {
				frame.phase = legacyNativeCheckpointGoGraphWaiting
				err = work.push(ctx, sample.source)
			}
		case legacyNativeCheckpointGoGraphAttach:
			err = work.attach(ctx, frame)
		case legacyNativeCheckpointGoGraphFinish:
			err = work.finish(ctx, frame)
		case legacyNativeCheckpointGoGraphPrepareBacking:
			pause = true
			err = work.prepareBacking(ctx, frame)
		default:
			err = ErrIntegrity
		}
	}
	if err != nil {
		return false, false, err
	}
	return work.ready, pause || work.verifying, nil
}

func (work *legacyNativeCheckpointGoGraph) fail(err error) (bool, error) {
	if work.failure == nil {
		err = work.registry.fail(err)
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointGoGraph) take(ctx context.Context) (legacyNativeAnyValue, error) {
	if work == nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeAnyValue{}, work.failure
	}
	if work.closed {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if ctx == nil {
		_, failure := work.fail(ErrIntegrity)
		return legacyNativeAnyValue{}, failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	if !work.ready || work.verifying || work.registry == nil || work.registry.active != work {
		_, failure := work.fail(ErrIntegrity)
		return legacyNativeAnyValue{}, failure
	}
	if err := work.validateResult(ctx); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	if err := work.admit(0, 0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	value := work.result
	work.close()
	return value, nil
}

func (work *legacyNativeCheckpointGoGraph) close() {
	if work == nil || work.closed {
		return
	}
	if work.registry != nil {
		if !work.ready {
			work.registry.fail(ErrIntegrity)
		} else if work.registry.active == work {
			work.registry.active = nil
		}
	}
	if work.resource != nil {
		work.resource.close()
	}
	if work.layout != nil {
		work.layout.close()
	}
	failure := work.failure
	*work = legacyNativeCheckpointGoGraph{closed: true, failure: failure}
}
