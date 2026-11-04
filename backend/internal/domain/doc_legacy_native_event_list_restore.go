package domain

import (
	"context"
	"reflect"
	"unsafe"
)

type legacyNativeEventListRestoreFrame struct {
	parent     *legacyNativeEventListRestoreFrame
	list       *legacyNativeEventList
	child      *legacyNativeEventList
	childState legacyNativeEventListCheckpointControl
	state      legacyNativeEventListCheckpointControl
	ref        legacyNativeCheckpointRef
	childRef   legacyNativeCheckpointRef
	walker     *legacyNativeEventList
	index      int
	walkIndex  int
	phase      uint8
}

func legacyNativeEventListRestoreFrameHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventListRestoreFrame{})))
	heap.allocation(int64(10*(len(contentHashPrefix)+64)), 1)
	return heap
}

type legacyNativeEventListCheckpointRestore struct {
	current  *legacyNativeEventListRestoreFrame
	scope    *legacyNormalizationMutation
	scanner  *legacyJSONValueScanner
	source   LegacyByteSource
	binding  ContentHash
	store    legacyNativeCheckpointIO
	values   *legacyNativeEventValuesCheckpointRestore
	object   *legacyNativeObjectCheckpointRestore
	result   *legacyNativeEventList
	state    legacyNativeEventListCheckpointControl
	decoded  docRangeHeaderHeapEstimate
	activity int64
	frames   int
	budget   int64
	discover bool
	ready    bool
	closed   bool
	failure  error
}

func newLegacyNativeEventListCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, parent *legacyNativeObjectCheckpointRestore, budget int64) (*legacyNativeEventListCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent == nil || parent.native == nil || parent.owner.list != nil || parent.eventFrame == nil || !ref.valid() || budget <= 0 || parent.closed {
		return nil, ErrIntegrity
	}
	if err := parent.owner.validate(parent.ownerGranted); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventListCheckpointRestore{})))
	planned.include(legacyNativeEventListRestoreFrameHeap())
	planned.allocation(4*LegacyCanonicalEncodingBytes, 1)
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	return &legacyNativeEventListCheckpointRestore{current: &legacyNativeEventListRestoreFrame{ref: ref}, scope: parent.native.mutation,
		scanner: parent.eventFrame, source: parent.source, binding: parent.binding, store: parent.store,
		activity: parent.native.mutation.activity.Load(), frames: 1, budget: budget, discover: true}, nil
}

func (work *legacyNativeEventListCheckpointRestore) checkSource() error {
	if work.closed || work.scope == nil || work.scope.closed.Load() {
		return ErrConflict
	}
	if work.scope.stepping.Load() || work.scope.activity.Load() != work.activity || work.activity != 0 && work.activity != -2 {
		return ErrLegacyNormalizationBusy
	}
	return nil
}

func (work *legacyNativeEventListCheckpointRestore) checkOwner() error {
	if err := work.checkSource(); err != nil {
		return err
	}
	if work.current == nil || work.current.phase != 4 || work.current.state.Child != 2 || work.scanner == nil {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventListCheckpointRestore) objectEventList(ref legacyNativeCheckpointRef) (*legacyNativeEventList, legacyNativeEventListCheckpointControl, error) {
	if err := work.checkOwner(); err != nil {
		return nil, legacyNativeEventListCheckpointControl{}, err
	}
	frame := work.current
	if frame.child == nil || ref != frame.childRef {
		return nil, legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	return frame.child, frame.childState, nil
}

func (work *legacyNativeEventListCheckpointRestore) discoverNext(ctx context.Context) error {
	frame := work.current
	if frame.phase == 0 {
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, frame.ref, &frame.state); err != nil {
			return err
		}
		if err := frame.state.validate(work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		if frame.state.Index != frame.index || frame.state.Parent != -1 && (frame.parent == nil || frame.state.Parent != frame.parent.index) ||
			frame.parent != nil && frame.state.ParentDepth != frame.parent.state.ParentDepth+2 {
			return ErrIntegrity
		}
		if err := work.admit(int64(unsafe.Sizeof(legacyNativeEventList{}))); err != nil {
			return err
		}
		state := frame.state
		list := &legacyNativeEventList{mutation: work.scope, paged: true, position: state.Position, end: state.End,
			parentDepth: state.ParentDepth, maximum: state.Maximum, count: state.Count, childStart: state.ChildStart,
			literal: state.Literal, phase: state.Phase, first: state.First, prefix: state.Prefix}
		if !state.DecodedNil {
			list.decoded = []CIREvent{}
		}
		if state.Parent != -1 {
			list.parent = frame.parent.list
		}
		if state.HasFrame {
			list.frame = work.scanner
		}
		frame.list, frame.phase = list, 1
		work.decoded.allocation(1, int64(unsafe.Sizeof(*list)))
		return work.decoded.err
	}
	if frame.phase != 1 {
		return ErrIntegrity
	}
	if frame.state.Child == 2 {
		var object legacyNativeObjectCheckpointControl
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, frame.state.Object, &object); err != nil {
			return err
		}
		if err := object.validate(legacyNativeCheckpointEventModel, work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		if object.ParentDepth != frame.state.ParentDepth+1 || !object.TrackPresence || !object.PagedEvents || object.End > frame.state.End || object.Position < frame.state.ChildStart ||
			object.Prefix && (frame.state.First != '{' || object.End != frame.state.End || frame.state.Position != frame.state.ChildStart) ||
			!object.Prefix && (frame.state.First != 'n' || object.End != frame.state.Position) {
			return ErrIntegrity
		}
		for _, field := range object.Fields {
			if field.Event != nil && field.Event.List != (legacyNativeCheckpointRef{}) {
				if frame.childRef != (legacyNativeCheckpointRef{}) {
					return ErrIntegrity
				}
				frame.childRef = field.Event.List
			}
		}
	}
	frame.phase = 2
	if frame.childRef == (legacyNativeCheckpointRef{}) {
		work.discover = false
		return nil
	}
	if work.frames >= 10000 {
		return ErrIntegrity
	}
	if err := work.admit(legacyNativeEventListRestoreFrameHeap().bytes); err != nil {
		return err
	}
	work.current = &legacyNativeEventListRestoreFrame{parent: frame, ref: frame.childRef, index: frame.index + 1}
	work.frames++
	return nil
}

func (work *legacyNativeEventListCheckpointRestore) advance(ctx context.Context) error {
	if work.discover {
		return work.discoverNext(ctx)
	}
	frame := work.current
	switch frame.phase {
	case 2:
		if work.values == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.values, err = newLegacyNativeEventValuesCheckpointRestore(ctx, frame.state.Data, work.binding, work.source.Bytes, work.store, remaining)
			return err
		}
		ready, err := work.values.step(ctx)
		if err != nil || !ready {
			return err
		}
		result, err := work.values.take(ctx)
		work.values = nil
		if err != nil {
			return err
		}
		if (result.Values != nil) != frame.state.HasValues || result.Values != nil && result.Values.count != frame.state.Count {
			return ErrIntegrity
		}
		frame.list.values, frame.list.excess = result.Values, result.Excess
		frame.list.refreshHeap()
		work.decoded.include(result.Values.heap())
		frame.phase++
		return work.decoded.err
	case 3:
		if frame.state.HasFrame {
			body, err := legacyNativeCheckpointRead(ctx, work.store, frame.state.Frame)
			if err != nil {
				return err
			}
			scanner, err := restoreLegacyJSONValueScanner(ctx, body)
			if err != nil || scanner != *work.scanner {
				return ErrIntegrity
			}
		}
		frame.phase++
	case 4:
		if frame.state.Child == 1 {
			var planned docRangeHeaderHeapEstimate
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObject{})))
			planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
			planned.allocation(int64(len(legacyEventNativeFields.fields)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})+unsafe.Sizeof(int64(0))))
			planned.allocation(int64(legacyEventNativeFields.keyBytes), 1)
			if err := work.admit(planned.bytes); err != nil {
				return err
			}
			object, err := newLegacyNativeObject(reflect.ValueOf(&frame.list.child), &legacyEventNativeFields, frame.state.ParentDepth+1, true)
			if err != nil {
				return err
			}
			object.mutation, object.pagedEvents, object.field = work.scope, true, -1
			frame.list.object = object
			retained, err := object.retainedBase()
			work.decoded.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
			frame.phase++
			return work.decoded.err
		}
		if frame.state.Child != 2 {
			frame.phase++
			return nil
		}
		if work.object == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.object, err = newLegacyNativeObjectCheckpointRestoreForOwner(ctx, frame.state.Object, legacyNativeCheckpointEventModel, work.store, work.source, work.binding, remaining, legacyNativeObjectCheckpointOwner{list: work})
			return err
		}
		ready, err := work.object.step(ctx)
		if err != nil || !ready {
			return err
		}
		result, err := work.object.take(ctx)
		work.object = nil
		if err != nil {
			return err
		}
		if result.Scanner != *work.scanner || result.Source.authority != work.source.authority || result.Object.destination.Interface() != &frame.list.child {
			return ErrIntegrity
		}
		frame.list.object, work.source = result.Object, result.Source
		retained, err := result.Object.retainedBase()
		if frame.child != nil {
			if retained < frame.child.borrowed.bytes {
				return ErrIntegrity
			}
			retained -= frame.child.borrowed.bytes
		}
		work.decoded.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		frame.phase++
		return work.decoded.err
	case 5:
		frame.walker, frame.walkIndex, frame.phase = frame.list, frame.index, 6
	case 6:
		if frame.state.Cursor != -1 {
			if frame.walkIndex != frame.state.Cursor {
				next, err := legacyNativeEventListCheckpointChild(frame.walker)
				if err != nil || next == nil || next.parent != frame.walker || frame.walkIndex >= 9999 {
					return ErrIntegrity
				}
				frame.walker, frame.walkIndex = next, frame.walkIndex+1
				return nil
			}
			frame.list.cursor = frame.walker
		}
		if _, err := legacyNativeEventListCheckpointControlFor(frame.list, work.binding, work.source.Bytes, frame.index, frame.state.Parent, frame.state.Cursor); err != nil {
			return err
		}
		work.current, work.frames = frame.parent, work.frames-1
		if work.current == nil {
			work.result, work.state, work.ready = frame.list, frame.state, true
		} else {
			work.current.child, work.current.childState = frame.list, frame.state
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventListCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.decoded)
	heap.include(docRangeHeaderHeapEstimate{bytes: int64(work.frames) * legacyNativeEventListRestoreFrameHeap().bytes})
	heap.allocation(int64(len(work.binding)), 1)
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.values, work.object} {
		retained, err := current.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventListCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeEventListCheckpointRestore) admit(extra int64) error {
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

func (work *legacyNativeEventListCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
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

func (work *legacyNativeEventListCheckpointRestore) take(ctx context.Context) (*legacyNativeEventList, legacyNativeEventListCheckpointControl, error) {
	if work == nil {
		return nil, legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if work.failure != nil {
		return nil, legacyNativeEventListCheckpointControl{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, legacyNativeEventListCheckpointControl{}, err
	}
	if !work.ready || work.closed {
		work.fail(ErrIntegrity)
		return nil, legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return nil, legacyNativeEventListCheckpointControl{}, err
	}
	result, state := work.result, work.state
	work.close()
	return result, state, nil
}

func (work *legacyNativeEventListCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventListCheckpointRestore) close() {
	if work != nil && !work.closed {
		work.values.close()
		work.object.close()
		failure := work.failure
		*work = legacyNativeEventListCheckpointRestore{closed: true, failure: failure}
	}
}
