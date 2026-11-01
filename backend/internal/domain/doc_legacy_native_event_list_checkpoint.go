package domain

import (
	"context"
	"unsafe"
)

type legacyNativeEventListCheckpointFrame struct {
	parent     *legacyNativeEventListCheckpointFrame
	list       *legacyNativeEventList
	childList  *legacyNativeEventList
	childRef   legacyNativeCheckpointRef
	shape      legacyNativeEventListCheckpointControl
	state      legacyNativeEventListCheckpointControl
	walker     *legacyNativeEventList
	index      int
	walkIndex  int
	phase      uint8
	shapeReady bool
}

func legacyNativeEventListCheckpointFrameHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventListCheckpointFrame{})))
	heap.allocation(int64(8*(len(contentHashPrefix)+64)), 1)
	return heap
}

func legacyNativeEventListCheckpointChild(list *legacyNativeEventList) (*legacyNativeEventList, error) {
	if list == nil || list.object == nil {
		return nil, nil
	}
	object := list.object
	if object.events != nil && len(object.events) != len(legacyEventNativeFields.fields) {
		return nil, ErrIntegrity
	}
	var next *legacyNativeEventList
	for index, child := range object.events {
		if child != nil {
			if next != nil || !legacyEventNativeFields.fields[index].eventList || child.mutation != list.mutation || child.parentDepth != list.parentDepth+2 {
				return nil, ErrIntegrity
			}
			next = child
		}
	}
	return next, nil
}

type legacyNativeEventListCheckpoint struct {
	list         *legacyNativeEventList
	scanner      *legacyJSONValueScanner
	scannerShape legacyJSONValueScanner
	capture      *legacyNormalizationCapture
	source       LegacyByteSource
	binding      ContentHash
	store        legacyNativeCheckpointIO
	current      *legacyNativeEventListCheckpointFrame
	values       *legacyNativeEventValuesCheckpoint
	object       *legacyNativeObjectCheckpoint
	root         legacyNativeCheckpointRef
	sourceBytes  int64
	frames       int
	budget       int64
	discovering  bool
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeEventListCheckpoint(ctx context.Context, parent *legacyNativeObjectCheckpoint, field int, available int64) (*legacyNativeEventListCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent == nil || parent.eventOwner != nil || parent.closed || !parent.capture.valid(parent.native.mutation) || field < 0 || field >= len(parent.native.metadata.fields) || !parent.native.metadata.fields[field].eventList || len(parent.native.events) != len(parent.native.metadata.fields) || available <= 0 {
		return nil, ErrIntegrity
	}
	list := parent.native.events[field]
	if list == nil || list.parent != nil || list.mutation != parent.native.mutation {
		return nil, ErrIntegrity
	}
	var source docRangeHeaderHeapEstimate
	list.retained(&source)
	source.include(list.borrowed)
	if source.err != nil || source.bytes > parent.eventSource {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventListCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.include(legacyNativeEventListCheckpointFrameHeap())
	planned.include(legacyNativeAnyCheckpointSourceHeap(parent.source))
	planned.include(docRangeHeaderHeapEstimate{bytes: 4 * LegacyCanonicalEncodingBytes})
	if planned.err != nil || planned.bytes > available {
		return nil, ErrDocumentResourceLimit
	}
	capture, err := list.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	budget := docRangeHeaderHeapEstimate{bytes: available}
	budget.include(source)
	if budget.err != nil {
		capture.release()
		return nil, budget.err
	}
	work := &legacyNativeEventListCheckpoint{list: list, scanner: parent.scanner, scannerShape: *parent.scanner, capture: capture,
		source: parent.source, binding: parent.state.Binding, store: parent.store, sourceBytes: source.bytes, budget: budget.bytes,
		current: &legacyNativeEventListCheckpointFrame{list: list}, frames: 1, discovering: true}
	return work, nil
}

func (work *legacyNativeEventListCheckpoint) checkSource() error {
	if work.capture == nil || !work.capture.valid(work.list.mutation) || work.scanner == nil || *work.scanner != work.scannerShape {
		return ErrConflict
	}
	if work.current != nil && work.current.shapeReady {
		frame := work.current
		shape, err := legacyNativeEventListCheckpointControlFor(frame.list, work.binding, work.source.Bytes, frame.index, frame.shape.Parent, frame.shape.Cursor)
		if err != nil {
			return err
		}
		if shape != frame.shape {
			return ErrConflict
		}
	}
	return nil
}

func (work *legacyNativeEventListCheckpoint) checkObjectSource(native *legacyNativeObject, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource) error {
	if err := work.checkSource(); err != nil {
		return err
	}
	frame := work.current
	if frame == nil || frame.phase != 4 || frame.state.Child != 2 || native != frame.list.object || scanner != frame.list.frame || binding != work.binding || source.authority != work.source.authority || source.Bytes != work.source.Bytes || source.partBytes() != work.source.partBytes() || native.mutation != work.list.mutation {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventListCheckpoint) objectEventReference(list *legacyNativeEventList) (legacyNativeCheckpointRef, error) {
	if err := work.checkSource(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	frame := work.current
	if frame == nil || frame.phase != 4 || frame.childList != list || !frame.childRef.valid() {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	return frame.childRef, nil
}

func (work *legacyNativeEventListCheckpoint) advance(ctx context.Context) error {
	frame := work.current
	if frame == nil {
		return ErrIntegrity
	}
	if work.discovering {
		next, err := legacyNativeEventListCheckpointChild(frame.list)
		if err != nil {
			return err
		}
		if next == nil {
			work.discovering = false
			return nil
		}
		if work.frames >= 10000 || next.parent != nil && next.parent != frame.list {
			return ErrIntegrity
		}
		if err := work.admit(legacyNativeEventListCheckpointFrameHeap().bytes); err != nil {
			return err
		}
		frame.childList = next
		work.current = &legacyNativeEventListCheckpointFrame{parent: frame, list: next, index: frame.index + 1}
		work.frames++
		return nil
	}
	switch frame.phase {
	case 0:
		frame.walker, frame.walkIndex, frame.phase = frame.list, frame.index, 1
	case 1:
		cursor := frame.list.cursor
		cursorIndex := -1
		if cursor != nil {
			if cursor != frame.walker {
				next, err := legacyNativeEventListCheckpointChild(frame.walker)
				if err != nil {
					return err
				}
				if next == nil || next.parent != frame.walker || frame.walkIndex >= 9999 {
					return ErrIntegrity
				}
				frame.walker, frame.walkIndex = next, frame.walkIndex+1
				return nil
			}
			cursorIndex = frame.walkIndex
		}
		parentIndex := -1
		if frame.list.parent != nil {
			if frame.parent == nil || frame.list.parent != frame.parent.list {
				return ErrIntegrity
			}
			parentIndex = frame.parent.index
		}
		state, err := legacyNativeEventListCheckpointControlFor(frame.list, work.binding, work.source.Bytes, frame.index, parentIndex, cursorIndex)
		if err != nil {
			return err
		}
		if frame.list.frame != nil && (frame.list.frame != work.scanner || *frame.list.frame != work.scannerShape) {
			return ErrIntegrity
		}
		frame.shape, frame.state, frame.shapeReady, frame.phase = state, state, true, 2
	case 2:
		if work.values == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			heap := frame.list.values.heap()
			if heap.err != nil || heap.bytes > work.sourceBytes {
				return ErrIntegrity
			}
			work.values, err = newLegacyNativeEventValuesCheckpoint(ctx, frame.list, work.binding, work.source.Bytes, work.store, remaining+heap.bytes)
			return err
		}
		ready, err := work.values.step(ctx)
		if err != nil || !ready {
			return err
		}
		frame.state.Data, err = work.values.take(ctx)
		work.values = nil
		if err != nil {
			return err
		}
		frame.phase++
	case 3:
		if frame.state.HasFrame {
			body, err := frame.list.frame.checkpoint(ctx)
			if err != nil {
				return err
			}
			frame.state.Frame, err = legacyNativeCheckpointWrite(ctx, work.store, body)
			if err != nil {
				return err
			}
		}
		frame.phase++
	case 4:
		if frame.state.Child != 2 {
			frame.phase++
			return nil
		}
		if work.object == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.object, err = newLegacyNativeObjectCheckpointForEvents(ctx, frame.list.object, frame.list.frame, work.binding, work.source, work.store, remaining, work)
			return err
		}
		ready, err := work.object.step(ctx)
		if err != nil || !ready {
			return err
		}
		frame.state.Object, err = work.object.take(ctx)
		work.object = nil
		if err != nil {
			return err
		}
		frame.phase++
	case 5:
		if err := frame.state.validate(work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, frame.state)
		if err != nil {
			return err
		}
		work.current, work.frames = frame.parent, work.frames-1
		if work.current == nil {
			work.root, work.ready = ref, true
		} else {
			work.current.childRef = ref
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventListCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.binding)+len(work.root.Hash)), 1)
	if work.frames < 0 || work.frames > 10000 {
		return 0, ErrIntegrity
	}
	unit := legacyNativeEventListCheckpointFrameHeap()
	heap.include(docRangeHeaderHeapEstimate{bytes: int64(work.frames) * unit.bytes, err: unit.err})
	if work.values != nil {
		retained, err := work.values.retainedBytes()
		if retained < work.values.sourceHeap.bytes {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - work.values.sourceHeap.bytes, err: err})
	}
	if work.object != nil {
		retained, err := work.object.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventListCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget || work.sourceBytes > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - work.sourceBytes, nil
}

func (work *legacyNativeEventListCheckpoint) admit(extra int64) error {
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

func (work *legacyNativeEventListCheckpoint) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
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

func (work *legacyNativeEventListCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	root := work.root
	work.close()
	return root, nil
}

func (work *legacyNativeEventListCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventListCheckpoint) close() {
	if work != nil && !work.closed {
		work.values.close()
		work.object.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNativeEventListCheckpoint{closed: true, failure: failure}
	}
}
