package domain

import (
	"context"
	"reflect"
	"unsafe"
	"weak"
)

type legacyNativeCheckpointArrayView struct {
	Backing legacyNativeCheckpointValue `json:"b"`
	Offset  uint64                      `json:"o"`
}

func (list *legacyNativeAnyArray) validateResource() error {
	if list.resource == nil {
		if list.offset != 0 {
			return ErrIntegrity
		}
		return nil
	}
	if err := list.resource.valid(); err != nil {
		return err
	}
	backing := list.resource.root.owner.array
	if backing == nil || backing == list || backing.resource != nil || list.previous != nil || list.offset < 0 || list.offset > cap(backing.decoded) || cap(list.decoded) > cap(backing.decoded)-list.offset {
		return ErrIntegrity
	}
	if cap(list.decoded) > 0 && reflect.ValueOf(list.decoded).Pointer() != reflect.ValueOf(backing.decoded[:cap(backing.decoded)][list.offset:]).Pointer() {
		return ErrIntegrity
	}
	return nil
}

func newLegacyNativeResourceArray(ctx context.Context, owner *legacyNativeResourceOwner, offset, length, capacity int) (*legacyNativeAnyArray, error) {
	view, err := owner.view(ctx, offset, length, capacity)
	if err != nil {
		return nil, err
	}
	values, err := view.values(ctx)
	if err != nil {
		return nil, err
	}
	list, err := newLegacyNativeAnyArray(capacity)
	if err != nil {
		return nil, err
	}
	list.decoded, list.tracked = values[:0], true
	list.resource, list.offset = owner, offset
	list.estimate.include(legacyNativeAnyArrayBacking(capacity))
	return list, nil
}

func (list *legacyNativeAnyArray) resourceValue(ctx context.Context, position int) (legacyNativeAnyValue, error) {
	if err := list.validateResource(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if list.resource == nil || position < 0 || position >= cap(list.decoded) {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	root := list.resource.root
	physical := list.offset + position
	if physical < len(root.owner.array.decoded) {
		return root.owner.arrayValue(ctx, physical)
	}
	if root.owner.array.tail != nil {
		return root.owner.array.tailValue(ctx, physical)
	}
	return legacyNativeAnyValue{}, ctx.Err()
}

func (list *legacyNativeAnyArray) finishResource() {
	list.resourceRead = weak.Make(list.resource.capture)
}

func newLegacyNativeResourcePreparationForGoGraph(ctx context.Context, graph *legacyNativeCheckpointGoGraph, root legacyNativeAnyValue) (*legacyNativeResourcePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if graph == nil || graph.closed || graph.resource != nil || graph.frame == nil || graph.registry == nil || graph.registry.active != graph || root.owner.array == nil || root.owner.array.resource != nil {
		return nil, ErrConflict
	}
	frame := graph.frame
	if frame.phase != legacyNativeCheckpointGoGraphPrepareBacking || frame.group == nil || frame.group.entry == nil || !frame.group.entry.complete || frame.group.entry.value.owner != root.owner || frame.child.owner != root.owner {
		return nil, ErrConflict
	}
	if err := root.validateOwnership(); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeResourcePreparation{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.include(root.heap)
	planned.include(docRangeHeaderHeapEstimate{bytes: root.owner.bytes})
	if planned.err != nil {
		return nil, planned.err
	}
	if err := graph.admit(planned.bytes, 0); err != nil {
		return nil, err
	}
	if graph.registry.backingCapture == nil {
		scope := &legacyNormalizationMutation{}
		capture, err := scope.beginCapture()
		if err != nil {
			return nil, err
		}
		graph.registry.backingCapture = capture
	}
	capture, err := graph.registry.backingCapture.scope.beginCapture()
	if err != nil {
		return nil, err
	}
	work := &legacyNativeResourcePreparation{graph: graph, root: root, capture: capture}
	graph.resource = work
	if _, err := work.enroll(root.owner); err != nil {
		graph.resource = nil
		work.close()
		return nil, err
	}
	work.current = work.first
	return work, nil
}

func (work *legacyNativeCheckpointGoGraph) prepareBacking(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	if frame.group == nil || frame.group.entry == nil || frame.group.owner != nil || !frame.group.preparing || frame.child.owner != frame.group.entry.value.owner {
		return ErrIntegrity
	}
	if work.resource == nil {
		_, err := newLegacyNativeResourcePreparationForGoGraph(ctx, work, frame.child)
		return err
	}
	ready, err := work.resource.step(ctx)
	if err != nil || !ready {
		return err
	}
	owner, err := work.resource.take(ctx)
	if err != nil {
		return err
	}
	work.resource.close()
	work.resource = nil
	frame.group.owner, frame.group.preparing = owner, false
	frame.group.entry.resource = owner
	if err := work.registry.account(frame.group.entry); err != nil {
		return err
	}
	frame.child, frame.phase = legacyNativeAnyValue{}, legacyNativeCheckpointGoGraphStart
	return nil
}

func newLegacyNativeResourcePreparationForRestore(ctx context.Context, restore *legacyNativeCheckpointGraphRestore, root legacyNativeAnyValue) (*legacyNativeResourcePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if restore == nil || restore.closed || restore.resource != nil || restore.frame == nil || root.owner.array == nil || root.owner.array.resource != nil {
		return nil, ErrConflict
	}
	frame := restore.frame
	if frame.phase != legacyNativeCheckpointGraphRestoreResource || frame.backing == nil || frame.backing.active || frame.backing.value.owner != root.owner || frame.pending.owner != root.owner {
		return nil, ErrConflict
	}
	if err := root.validateOwnership(); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeResourcePreparation{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.include(root.heap)
	planned.include(docRangeHeaderHeapEstimate{bytes: root.owner.bytes})
	if planned.err != nil {
		return nil, planned.err
	}
	if err := restore.admit(planned.bytes, 0); err != nil {
		return nil, err
	}
	if restore.registry.capture == nil {
		scope := &legacyNormalizationMutation{}
		capture, err := scope.beginCapture()
		if err != nil {
			return nil, err
		}
		restore.registry.capture = capture
	}
	scope := restore.registry.capture.scope
	capture, err := scope.beginCapture()
	if err != nil {
		return nil, err
	}
	work := &legacyNativeResourcePreparation{restore: restore, root: root, capture: capture}
	restore.resource = work
	if _, err := work.enroll(root.owner); err != nil {
		restore.resource = nil
		work.close()
		return nil, err
	}
	work.current = work.first
	return work, nil
}

func (work *legacyNativeCheckpointGraphRestore) prepareResource(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	if frame.backing == nil {
		state, err := legacyNativeCheckpointGraphRestoreDescriptor(ctx, frame.record.Allocation.Backing)
		if err != nil || state.Kind != legacyNativeCheckpointArray {
			return ErrIntegrity
		}
		entry, missing := work.registry.lookup(state.Reference)
		if missing != 0 || entry == nil || entry.active || entry.array == nil || entry.array.resource != nil || entry.value.owner != frame.pending.owner {
			return ErrIntegrity
		}
		frame.backing = entry
	}
	if frame.backing.resource != nil {
		frame.pending, frame.phase = legacyNativeAnyValue{}, legacyNativeCheckpointGraphRestoreAllocate
		return work.refresh(frame)
	}
	if work.resource == nil {
		_, err := newLegacyNativeResourcePreparationForRestore(ctx, work, frame.pending)
		return err
	}
	ready, err := work.resource.step(ctx)
	if err != nil || !ready {
		return err
	}
	owner, err := work.resource.take(ctx)
	if err != nil {
		return err
	}
	work.resource.close()
	work.resource = nil
	frame.backing.resource = owner
	metadata := owner.metadataHeap()
	if metadata.err != nil {
		return metadata.err
	}
	work.registry.owned += metadata.bytes
	frame.pending, frame.phase = legacyNativeAnyValue{}, legacyNativeCheckpointGraphRestoreAllocate
	return work.refresh(frame)
}

func (owner *legacyNativeResourceOwner) metadataHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*owner)))
	legacyCanonicalStorageAllocation(&heap, int(owner.count), int64(unsafe.Sizeof(legacyNativeResourceNode{})))
	return heap
}

func (list *legacyNativeAnyArray) viewMetadataHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*list)))
	list.children.retained(&heap)
	if list.tail != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*list.tail)))
		list.tail.children.retained(&heap)
	}
	return heap
}
