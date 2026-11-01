package domain

import (
	"context"
	"reflect"
	"runtime"
	"unsafe"
	"weak"
)

func legacyNativeResourceReadStatus(pointer weak.Pointer[legacyNormalizationCapture]) error {
	capture := pointer.Value()
	if capture == nil || capture.references.Load() <= 0 {
		return nil
	}
	if capture.scope == nil || capture.scope.closed.Load() {
		return ErrConflict
	}
	if capture.valid(capture.scope) {
		return ErrLegacyNormalizationBusy
	}
	return nil
}

type legacyNativeResourceNode struct {
	next     *legacyNativeResourceNode
	owner    legacyNativeAnyValueOwner
	bytes    int64
	complete bool
}

type legacyNativeResourceLookup struct {
	children [16]*legacyNativeResourceLookup
	node     *legacyNativeResourceNode
}

type legacyNativeResourcePreparation struct {
	parent      *legacyNativeObjectCheckpoint
	root        legacyNativeAnyValue
	capture     *legacyNormalizationCapture
	index       *legacyNativeResourceLookup
	first       *legacyNativeResourceNode
	last        *legacyNativeResourceNode
	current     *legacyNativeResourceNode
	verify      *legacyNativeResourceNode
	cursor      *legacyNativeAnyMapCheckpoint
	result      *legacyNativeResourceOwner
	position    int
	count       int64
	indices     int64
	bytes       int64
	verifying   bool
	ready       bool
	transferred bool
	closed      bool
	failure     error
}

func legacyNativeResourceHeap(owner legacyNativeAnyValueOwner) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if err := owner.validate(); err != nil {
		heap.err = err
		return heap
	}
	if owner.object != nil && owner.array == nil {
		heap = owner.object.heap()
		owner.object.retained(&heap)
	} else if owner.array != nil && owner.object == nil && owner.array.previous == nil {
		heap = owner.array.heap()
		owner.array.retained(&heap)
	} else {
		heap.err = ErrIntegrity
	}
	return heap
}

func newLegacyNativeResourcePreparation(ctx context.Context, parent *legacyNativeObjectCheckpoint, registry *legacyNativeCheckpointGoGraphRegistry, key legacyNativeCheckpointGoGraphKey) (*legacyNativeResourcePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent == nil || parent.resourceTask != nil || registry == nil || registry.closed || registry.source == nil ||
		registry.source.owner != parent || registry.source.registry != registry || parent.initialTask != registry.source {
		return nil, ErrConflict
	}
	if err := registry.source.checkSource(); err != nil {
		return nil, err
	}
	entry, err := registry.lookup(key)
	if err != nil || entry == nil || entry.key != key || !entry.complete || entry.array == nil ||
		entry.value.owner.array != entry.array || entry.object != nil || entry.array.previous != nil {
		return nil, ErrIntegrity
	}
	if err := entry.value.validateOwnership(); err != nil {
		return nil, err
	}
	heap := entry.value.heap
	heap.include(docRangeHeaderHeapEstimate{bytes: entry.value.owner.bytes})
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeResourcePreparation{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	if heap.err != nil {
		return nil, heap.err
	}
	if err := parent.admit(heap.bytes); err != nil {
		return nil, err
	}
	capture, err := parent.native.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	work := &legacyNativeResourcePreparation{parent: parent, root: entry.value, capture: capture}
	parent.resourceTask = work
	if _, err := work.enroll(entry.value.owner); err != nil {
		parent.resourceTask = nil
		work.close()
		return nil, err
	}
	work.current = work.first
	return work, nil
}

func (work *legacyNativeResourcePreparation) checkSource() error {
	if work == nil || work.closed || work.transferred || work.parent == nil || work.parent.resourceTask != work ||
		work.parent.native == nil || !work.capture.valid(work.parent.native.mutation) {
		return ErrConflict
	}
	if err := work.parent.checkSource(); err != nil {
		return err
	}
	return work.root.validateOwnership()
}

func (work *legacyNativeResourcePreparation) admit(extra int64) error {
	if err := work.checkSource(); err != nil {
		return err
	}
	return work.parent.admit(extra)
}

func (work *legacyNativeResourcePreparation) enroll(owner legacyNativeAnyValueOwner) (bool, error) {
	heap := legacyNativeResourceHeap(owner)
	if heap.err != nil {
		return false, heap.err
	}
	address := reflect.ValueOf(owner.object).Pointer()
	if owner.array != nil {
		address = reflect.ValueOf(owner.array).Pointer()
	}
	const levels = int(unsafe.Sizeof(uintptr(0))) * 2
	lookup, missing := work.index, levels+1
	if lookup != nil {
		missing = 0
		for level := levels - 1; level >= 0; level-- {
			lookup = lookup.children[(address>>uint(4*level))&15]
			if lookup == nil {
				missing = level + 1
				break
			}
		}
	}
	if missing == 0 {
		if lookup.node == nil || lookup.node.owner != owner {
			return false, ErrConflict
		}
		return false, nil
	}
	if uint64(work.count) >= legacyNativeCheckpointNodes {
		return false, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	legacyCanonicalStorageAllocation(&planned, missing, int64(unsafe.Sizeof(legacyNativeResourceLookup{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeResourceNode{})))
	if planned.err != nil {
		return false, planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return false, err
	}
	var previous *legacyNormalizationCapture
	if owner.object != nil {
		previous = owner.object.resourceRead.Value()
	} else {
		previous = owner.array.resourceRead.Value()
	}
	if previous != nil && previous != work.capture && previous.references.Load() > 0 {
		return false, ErrConflict
	}
	if owner.object != nil {
		owner.object.resourceRead = weak.Make(work.capture)
	} else {
		owner.array.resourceRead = weak.Make(work.capture)
	}
	if work.index == nil {
		work.index = &legacyNativeResourceLookup{}
		work.indices++
	}
	lookup = work.index
	for level := levels - 1; level >= 0; level-- {
		position := (address >> uint(4*level)) & 15
		if lookup.children[position] == nil {
			lookup.children[position] = &legacyNativeResourceLookup{}
			work.indices++
		}
		lookup = lookup.children[position]
	}
	node := &legacyNativeResourceNode{owner: owner, bytes: heap.bytes}
	lookup.node = node
	if work.last == nil {
		work.first = node
	} else {
		work.last.next = node
	}
	work.last = node
	work.count++
	total := docRangeHeaderHeapEstimate{bytes: work.bytes}
	total.include(heap)
	if total.err != nil {
		return false, total.err
	}
	work.bytes = total.bytes
	return true, nil
}

func (work *legacyNativeResourcePreparation) child(value legacyNativeAnyValue) error {
	if err := value.validateOwnership(); err != nil {
		return err
	}
	if value.owner.object == nil && value.owner.array == nil {
		return nil
	}
	heap := value.heap
	heap.include(docRangeHeaderHeapEstimate{bytes: value.owner.bytes})
	if heap.err != nil || heap.bytes > work.current.bytes || heap.bytes > work.bytes {
		return ErrIntegrity
	}
	if _, err := work.enroll(value.owner); err != nil {
		return err
	}
	work.current.bytes -= heap.bytes
	work.bytes -= heap.bytes
	return nil
}

func (work *legacyNativeResourcePreparation) advance(ctx context.Context) error {
	if work.verifying {
		if work.verify == nil {
			work.ready = true
			return nil
		}
		if !work.verify.complete {
			return ErrIntegrity
		}
		if err := work.verify.owner.validate(); err != nil {
			return err
		}
		work.verify = work.verify.next
		return nil
	}
	node := work.current
	if node == nil {
		work.verify, work.verifying = work.first, true
		return nil
	}
	if err := node.owner.validate(); err != nil {
		return err
	}
	if node.owner.object != nil {
		if work.cursor == nil {
			if err := work.admit(int64(unsafe.Sizeof(legacyNativeAnyMapCheckpoint{}))); err != nil {
				return err
			}
			cursor, err := node.owner.mapCheckpoint(ctx, true)
			if err != nil {
				return err
			}
			work.cursor = cursor
			return nil
		}
		entry, found, err := work.cursor.next(ctx)
		if err != nil {
			return err
		}
		if found {
			value := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
			if _, ok := entry.Value.(string); ok {
				value.capacity = len(entry.Value.(string)) + int(entry.ValueExcess/2)
			}
			return work.child(value)
		}
		work.cursor.close()
		work.cursor = nil
	} else if work.position < cap(node.owner.array.decoded) {
		array := node.owner.array
		var value legacyNativeAnyValue
		var err error
		if work.position < len(array.decoded) {
			value, err = node.owner.arrayValue(ctx, work.position)
		} else if array.tail != nil {
			value, err = array.tailValue(ctx, work.position)
		}
		if err != nil {
			return err
		}
		if err := work.child(value); err != nil {
			return err
		}
		work.position++
		return nil
	}
	node.complete = true
	work.current, work.position = node.next, 0
	return nil
}

func (work *legacyNativeResourcePreparation) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	for count := 0; count < 4 && !work.ready; count++ {
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.advance(ctx); err != nil {
			return work.fail(err)
		}
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeResourcePreparation) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	if work.result != nil {
		retained, err := work.result.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		return heap.bytes, heap.err
	}
	heap.include(work.root.heap)
	heap.include(docRangeHeaderHeapEstimate{bytes: work.root.owner.bytes})
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	legacyCanonicalStorageAllocation(&heap, int(work.indices), int64(unsafe.Sizeof(legacyNativeResourceLookup{})))
	legacyCanonicalStorageAllocation(&heap, int(work.count), int64(unsafe.Sizeof(legacyNativeResourceNode{})))
	if work.cursor != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.cursor)))
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeResourcePreparation) take(ctx context.Context) (*legacyNativeResourceOwner, error) {
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
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return nil, err
	}
	if !work.ready || work.transferred || work.bytes < 0 {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	if err := work.admit(int64(unsafe.Sizeof(legacyNativeResourceOwner{}))); err != nil {
		work.fail(err)
		return nil, err
	}
	result := &legacyNativeResourceOwner{capture: work.capture, root: work.root, nodes: work.first, count: work.count, bytes: work.bytes}
	work.capture, work.root, work.first, work.last = nil, legacyNativeAnyValue{}, nil, nil
	work.current, work.verify, work.index = nil, nil, nil
	work.count, work.indices = 0, 0
	work.result, work.transferred = result, true
	return result, nil
}

func (work *legacyNativeResourcePreparation) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeResourcePreparation) close() {
	if work != nil {
		if work.capture != nil {
			work.capture.release()
		}
		if work.cursor != nil {
			work.cursor.close()
		}
		failure := work.failure
		*work = legacyNativeResourcePreparation{closed: true, failure: failure}
	}
}

type legacyNativeResourceOwner struct {
	capture *legacyNormalizationCapture
	root    legacyNativeAnyValue
	nodes   *legacyNativeResourceNode
	count   int64
	bytes   int64
	closed  bool
}

func (owner *legacyNativeResourceOwner) valid() error {
	if owner == nil || owner.closed || owner.capture == nil || !owner.capture.valid(owner.capture.scope) {
		return ErrConflict
	}
	return owner.root.validateOwnership()
}

func (owner *legacyNativeResourceOwner) retainedBytes() (int64, error) {
	if owner == nil || owner.closed {
		return 0, nil
	}
	if err := owner.valid(); err != nil {
		return 0, err
	}
	if owner.count <= 0 || owner.nodes == nil || owner.bytes < 0 {
		return 0, ErrIntegrity
	}
	heap := docRangeHeaderHeapEstimate{bytes: owner.bytes}
	heap.allocation(1, int64(unsafe.Sizeof(*owner)))
	legacyCanonicalStorageAllocation(&heap, int(owner.count), int64(unsafe.Sizeof(legacyNativeResourceNode{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
	return heap.bytes, heap.err
}

func (owner *legacyNativeResourceOwner) close() {
	if owner != nil && !owner.closed {
		if owner.capture != nil {
			owner.capture.release()
		}
		*owner = legacyNativeResourceOwner{closed: true}
	}
}

type legacyNativeResourceView struct {
	owner    *legacyNativeResourceOwner
	offset   int
	length   int
	capacity int
}

func (owner *legacyNativeResourceOwner) view(ctx context.Context, offset, length, capacity int) (*legacyNativeResourceView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.valid(); err != nil {
		return nil, err
	}
	if owner.root.owner.array == nil || offset < 0 || length < 0 || capacity < length ||
		offset > cap(owner.root.owner.array.decoded) || capacity > cap(owner.root.owner.array.decoded)-offset {
		return nil, ErrIntegrity
	}
	return &legacyNativeResourceView{owner: owner, offset: offset, length: length, capacity: capacity}, nil
}

func (view *legacyNativeResourceView) values(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view == nil || view.owner == nil {
		return nil, ErrIntegrity
	}
	if err := view.owner.valid(); err != nil {
		return nil, err
	}
	backing := view.owner.root.owner.array.decoded[:cap(view.owner.root.owner.array.decoded)]
	values := backing[view.offset : view.offset+view.length : view.offset+view.capacity]
	runtime.KeepAlive(view.owner)
	return values, nil
}

func (view *legacyNativeResourceView) retainedBytes() (int64, error) {
	if view == nil || view.owner == nil {
		return 0, ErrIntegrity
	}
	retained, err := view.owner.retainedBytes()
	if view.owner.closed {
		return 0, ErrConflict
	}
	heap := docRangeHeaderHeapEstimate{bytes: retained, err: err}
	heap.allocation(1, int64(unsafe.Sizeof(*view)))
	return heap.bytes, heap.err
}
