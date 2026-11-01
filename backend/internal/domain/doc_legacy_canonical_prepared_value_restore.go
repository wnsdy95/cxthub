package domain

import (
	"context"
	"encoding/json"
	"math"
	"unsafe"
)

const (
	legacyCanonicalPreparedRestoreLoad uint8 = iota
	legacyCanonicalPreparedRestoreStart
	legacyCanonicalPreparedRestoreChild
	legacyCanonicalPreparedRestoreWaiting
	legacyCanonicalPreparedRestoreAppend
	legacyCanonicalPreparedRestoreTextStart
	legacyCanonicalPreparedRestoreTextNext
	legacyCanonicalPreparedRestoreTextRead
)

type legacyCanonicalPreparedValueRestoreFrame struct {
	parent       *legacyCanonicalPreparedValueRestoreFrame
	ref          legacyNativeCheckpointRef
	record       legacyCanonicalPreparedValueRecord
	list         *legacyCanonicalPreparedListReader[legacyNativeCheckpointRef]
	array        *legacyCanonicalPreparedArray
	pending      any
	texts        [2]string
	textCursor   *legacyNativeCheckpointTreeCursor
	textBuffer   []byte
	textRef      legacyNativeCheckpointRef
	textPosition int
	textSlot     int
	position     int
	storage      int64
	workspace    int64
	phase        uint8
}

type legacyCanonicalPreparedValueRestore struct {
	frame     *legacyCanonicalPreparedValueRestoreFrame
	numbers   *legacyCanonicalNumberValidation
	binding   ContentHash
	store     legacyNativeCheckpointIO
	result    any
	payload   docRangeHeaderHeapEstimate
	workspace int64
	budget    int64
	ready     bool
	closed    bool
	failure   error
}

func newLegacyCanonicalPreparedValueRestore(ctx context.Context, ref legacyNativeCheckpointRef, numbers *legacyCanonicalNumberValidation, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedValueRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueRestore{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueRestoreFrame{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	planned.include(legacyCanonicalPreparedValueWorkspace())
	if planned.err != nil {
		return nil, planned.err
	}
	if err := legacyCanonicalPreparedValueBudget(0, planned.bytes, budget); err != nil {
		return nil, err
	}
	work := &legacyCanonicalPreparedValueRestore{numbers: numbers, binding: binding, store: store, budget: budget}
	if err := work.push(ref); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalPreparedValueRestore) push(ref legacyNativeCheckpointRef) error {
	if !ref.valid() {
		return ErrIntegrity
	}
	frame := &legacyCanonicalPreparedValueRestoreFrame{parent: work.frame, ref: ref}
	work.frame = frame
	return work.refresh(frame)
}

func (work *legacyCanonicalPreparedValueRestore) refresh(frame *legacyCanonicalPreparedValueRestoreFrame) error {
	if frame == nil || frame.workspace > work.workspace {
		return ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*frame)))
	heap.allocation(int64(len(frame.ref.Hash)), 1)
	heap.allocation(int64(len(frame.record.Kind)), 1)
	heap.allocation(int64(len(frame.record.Binding)), 1)
	heap.allocation(int64(len(frame.record.Children.Ref.Hash)), 1)
	heap.allocation(int64(len(frame.textRef.Hash)), 1)
	for _, text := range frame.record.Texts {
		heap.allocation(int64(len(text.Tree.Ref.Hash)), 1)
	}
	if frame.list != nil {
		retained, err := frame.list.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if frame.textCursor != nil {
		retained, err := frame.textCursor.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if heap.err != nil {
		return heap.err
	}
	next := docRangeHeaderHeapEstimate{bytes: work.workspace - frame.workspace}
	next.include(heap)
	if next.err != nil {
		return next.err
	}
	work.workspace, frame.workspace = next.bytes, heap.bytes
	return nil
}

func (work *legacyCanonicalPreparedValueRestore) admit(extra docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	extra.include(legacyCanonicalPreparedValueWorkspace())
	if extra.err != nil {
		return extra.err
	}
	return legacyCanonicalPreparedValueBudget(retained, extra.bytes, work.budget)
}

func (work *legacyCanonicalPreparedValueRestore) complete(value any) error {
	frame := work.frame
	if frame == nil || frame.workspace > work.workspace || frame.textCursor != nil || frame.textBuffer != nil {
		return ErrIntegrity
	}
	if frame.list != nil {
		frame.list.close()
	}
	work.frame, work.workspace = frame.parent, work.workspace-frame.workspace
	if work.frame == nil {
		work.result, work.ready = value, true
		return nil
	}
	if work.frame.phase != legacyCanonicalPreparedRestoreWaiting {
		return ErrIntegrity
	}
	work.frame.pending, work.frame.phase = value, legacyCanonicalPreparedRestoreAppend
	return nil
}

func (work *legacyCanonicalPreparedValueRestore) leaf(frame *legacyCanonicalPreparedValueRestoreFrame) error {
	var planned docRangeHeaderHeapEstimate
	switch frame.record.Tag {
	case legacyCanonicalPreparedNil:
	case legacyCanonicalPreparedBool:
		planned.allocation(1, int64(unsafe.Sizeof(false)))
	case legacyCanonicalPreparedInt:
		planned.allocation(1, int64(unsafe.Sizeof(int(0))))
	case legacyCanonicalPreparedFloat:
		planned.allocation(1, int64(unsafe.Sizeof(float64(0))))
	case legacyCanonicalPreparedString:
		planned.allocation(1, int64(unsafe.Sizeof(string(""))))
	case legacyCanonicalPreparedBlock:
		planned.allocation(1, int64(unsafe.Sizeof(ContentBlock{})))
	case legacyCanonicalPreparedObjectTag:
		planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedObject{})))
	case legacyCanonicalPreparedReplacementTag:
		planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedReplacement{})))
	case legacyCanonicalPreparedNumber, legacyCanonicalPreparedNumberPointer:
		if work.numbers == nil || work.numbers.pending.at(frame.record.Number) == nil {
			return ErrIntegrity
		}
		planned.allocation(1, int64(unsafe.Sizeof(json.Number(""))))
	default:
		return ErrIntegrity
	}
	if err := work.admit(planned); err != nil {
		return err
	}
	work.payload.include(planned)
	if work.payload.err != nil {
		return work.payload.err
	}
	var value any
	switch frame.record.Tag {
	case legacyCanonicalPreparedBool:
		value = frame.record.Bits == 1
	case legacyCanonicalPreparedInt:
		value = int(int64(frame.record.Bits))
	case legacyCanonicalPreparedFloat:
		value = math.Float64frombits(frame.record.Bits)
	case legacyCanonicalPreparedString:
		value = frame.texts[0]
	case legacyCanonicalPreparedBlock:
		value = ContentBlock{Text: frame.texts[0], Type: frame.texts[1]}
	case legacyCanonicalPreparedObjectTag:
		value = &legacyCanonicalPreparedObject{count: frame.record.Count}
	case legacyCanonicalPreparedReplacementTag:
		value = &legacyCanonicalPreparedReplacement{count: frame.record.Count}
	case legacyCanonicalPreparedNumber:
		value = json.Number(*work.numbers.pending.at(frame.record.Number))
	case legacyCanonicalPreparedNumberPointer:
		number := new(json.Number)
		*number = json.Number(*work.numbers.pending.at(frame.record.Number))
		value = number
	}
	return work.complete(value)
}

func (work *legacyCanonicalPreparedValueRestore) startText(frame *legacyCanonicalPreparedValueRestoreFrame) error {
	limit := 1
	if frame.record.Tag == legacyCanonicalPreparedBlock {
		limit = 2
	}
	if frame.textSlot == limit {
		return work.leaf(frame)
	}
	text := frame.record.Texts[frame.textSlot]
	if text.Bytes == 0 {
		frame.textSlot++
		return nil
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(int64(text.Bytes), 1)
	if err := work.admit(planned); err != nil {
		return err
	}
	cursor, err := newLegacyNativeCheckpointTreeCursor(text.Tree, work.store)
	if err != nil {
		return err
	}
	frame.textCursor, frame.textBuffer = cursor, make([]byte, text.Bytes)
	work.payload.include(planned)
	frame.textPosition, frame.phase = 0, legacyCanonicalPreparedRestoreTextNext
	return work.refresh(frame)
}

func (work *legacyCanonicalPreparedValueRestore) nextText(ctx context.Context, frame *legacyCanonicalPreparedValueRestoreFrame) error {
	ref, count, found, err := frame.textCursor.next(ctx)
	if err != nil {
		return err
	}
	if found {
		length := min(LegacyCanonicalEncodingBytes, len(frame.textBuffer)-frame.textPosition)
		if length <= 0 || ref.Bytes != length || count != uint64(length) {
			return ErrIntegrity
		}
		frame.textRef, frame.phase = ref, legacyCanonicalPreparedRestoreTextRead
		return work.refresh(frame)
	}
	if frame.textPosition != len(frame.textBuffer) {
		return ErrIntegrity
	}
	frame.textCursor.close()
	frame.textCursor = nil
	frame.texts[frame.textSlot] = unsafe.String(unsafe.SliceData(frame.textBuffer), len(frame.textBuffer))
	frame.textBuffer = nil
	frame.textSlot++
	frame.phase = legacyCanonicalPreparedRestoreTextStart
	return work.refresh(frame)
}

func (work *legacyCanonicalPreparedValueRestore) append(frame *legacyCanonicalPreparedValueRestoreFrame) error {
	if frame.array == nil || frame.position >= frame.record.Count || frame.array.values.length() != frame.position {
		return ErrIntegrity
	}
	_, future, err := frame.array.values.appendRequirements()
	if err != nil {
		return err
	}
	if frame.storage > future.bytes || frame.storage > work.payload.bytes {
		return ErrIntegrity
	}
	planned := docRangeHeaderHeapEstimate{bytes: future.bytes - frame.storage}
	if frame.array.values.single != nil && frame.array.values.length() == 1 {
		planned.include(legacyCanonicalPagesHeap[any](true, false, 0, 0))
	} else if frame.array.values.inline != nil && frame.array.values.length() == legacyCanonicalInlineValues {
		planned.include(legacyCanonicalPagesHeap[any](false, true, 0, 0))
	}
	if err := work.admit(planned); err != nil {
		return err
	}
	if err := frame.array.values.append(frame.pending); err != nil {
		return err
	}
	work.payload.bytes = work.payload.bytes - frame.storage + future.bytes
	frame.storage = future.bytes
	frame.pending = nil
	frame.position++
	frame.phase = legacyCanonicalPreparedRestoreChild
	return nil
}

func (work *legacyCanonicalPreparedValueRestore) advance(ctx context.Context) error {
	frame := work.frame
	if frame == nil {
		return ErrIntegrity
	}
	switch frame.phase {
	case legacyCanonicalPreparedRestoreLoad:
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, frame.ref, &frame.record); err != nil {
			return err
		}
		if err := frame.record.validate(work.binding); err != nil {
			return err
		}
		frame.ref = legacyNativeCheckpointRef{}
		frame.phase = legacyCanonicalPreparedRestoreStart
		return work.refresh(frame)
	case legacyCanonicalPreparedRestoreStart:
		switch frame.record.Tag {
		case legacyCanonicalPreparedString, legacyCanonicalPreparedBlock:
			frame.phase = legacyCanonicalPreparedRestoreTextStart
			return nil
		case legacyCanonicalPreparedArrayTag:
			var planned docRangeHeaderHeapEstimate
			planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedArray{})))
			if err := work.admit(planned); err != nil {
				return err
			}
			list, err := newLegacyCanonicalPreparedListReader[legacyNativeCheckpointRef](frame.record.Children, work.store)
			if err != nil {
				return err
			}
			frame.list, frame.array = list, &legacyCanonicalPreparedArray{}
			work.payload.include(planned)
			frame.phase = legacyCanonicalPreparedRestoreChild
			return work.refresh(frame)
		default:
			return work.leaf(frame)
		}
	case legacyCanonicalPreparedRestoreChild:
		ref, found, err := frame.list.next(ctx)
		if err != nil {
			return err
		}
		if err := work.refresh(frame); err != nil {
			return err
		}
		if !found {
			if frame.position != frame.record.Count || frame.array.values.length() != frame.record.Count {
				return ErrIntegrity
			}
			return work.complete(frame.array)
		}
		if frame.position >= frame.record.Count || !ref.valid() {
			return ErrIntegrity
		}
		frame.phase = legacyCanonicalPreparedRestoreWaiting
		return work.push(ref)
	case legacyCanonicalPreparedRestoreAppend:
		return work.append(frame)
	case legacyCanonicalPreparedRestoreTextStart:
		return work.startText(frame)
	case legacyCanonicalPreparedRestoreTextNext:
		return work.nextText(ctx, frame)
	case legacyCanonicalPreparedRestoreTextRead:
		body, err := legacyNativeCheckpointRead(ctx, work.store, frame.textRef)
		if err != nil {
			return err
		}
		if len(body) > len(frame.textBuffer)-frame.textPosition {
			return ErrIntegrity
		}
		copy(frame.textBuffer[frame.textPosition:], body)
		frame.textPosition += len(body)
		frame.textRef = legacyNativeCheckpointRef{}
		frame.phase = legacyCanonicalPreparedRestoreTextNext
		return work.refresh(frame)
	default:
		return ErrIntegrity
	}
}

func (work *legacyCanonicalPreparedValueRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		retained, err := work.retainedBytes()
		if err == nil {
			err = legacyCanonicalPreparedValueBudget(retained, 0, work.budget)
		}
		if err != nil {
			return work.fail(err)
		}
		return true, nil
	}
	err := work.admit(docRangeHeaderHeapEstimate{})
	if err == nil {
		err = work.advance(ctx)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		retained, retainedErr := work.retainedBytes()
		err = retainedErr
		if err == nil {
			err = legacyCanonicalPreparedValueBudget(retained, 0, work.budget)
		}
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyCanonicalPreparedValueRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(docRangeHeaderHeapEstimate{bytes: work.workspace})
	heap.include(work.payload)
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedValueRestore) take(ctx context.Context) (any, docRangeHeaderHeapEstimate, error) {
	if work == nil {
		return nil, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	if work.failure != nil {
		return nil, docRangeHeaderHeapEstimate{}, work.failure
	}
	if work.closed {
		return nil, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, docRangeHeaderHeapEstimate{}, err
	}
	if !work.ready || work.frame != nil || work.workspace != 0 || work.payload.err != nil {
		work.fail(ErrIntegrity)
		return nil, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	retained, err := work.retainedBytes()
	if err == nil {
		err = legacyCanonicalPreparedValueBudget(retained, 0, work.budget)
	}
	if err != nil {
		work.fail(err)
		return nil, docRangeHeaderHeapEstimate{}, err
	}
	value, heap := work.result, work.payload
	work.close()
	return value, heap, nil
}

func (work *legacyCanonicalPreparedValueRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalPreparedValueRestore) close() {
	if work == nil || work.closed {
		return
	}
	for frame := work.frame; frame != nil; frame = frame.parent {
		if frame.list != nil {
			frame.list.close()
		}
		if frame.textCursor != nil {
			frame.textCursor.close()
		}
	}
	failure := work.failure
	*work = legacyCanonicalPreparedValueRestore{closed: true, failure: failure}
}
