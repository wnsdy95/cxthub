package domain

import (
	"context"
	"encoding/json"
	"unsafe"
)

type legacyCanonicalPreparedRestore struct {
	state     legacyCanonicalPreparedState
	encoding  *LegacyCanonicalEncoding
	store     legacyNativeCheckpointIO
	budget    int64
	numbers   *legacyCanonicalPreparedNumbersRestore
	keys      *legacyCanonicalPreparedListReader[legacyCanonicalPreparedKeysState]
	frames    *legacyCanonicalPreparedListReader[legacyCanonicalPreparedFrameState]
	fields    *legacyCanonicalPreparedFieldsRestore
	value     *legacyCanonicalPreparedValueRestore
	keyState  legacyCanonicalPreparedKeysState
	frameData legacyCanonicalPreparedFrameState
	frame     legacyCanonicalEncodingFrame
	phase     uint8
	subphase  uint8
	ready     bool
}

func newLegacyCanonicalPreparedRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if budget <= legacyNativeAnyCheckpointScratch || ValidateContentHash(binding) != nil {
		return nil, ErrDocumentResourceLimit
	}
	var state legacyCanonicalPreparedState
	if err := legacyNativeCheckpointReadJSON(ctx, store, ref, &state); err != nil {
		return nil, err
	}
	if state.Kind != "legacy-canonical-prepared" || state.Version != 1 || state.Binding != binding ||
		!state.Keys.valid() || !state.Frames.valid() || state.Frames.Count == 0 || state.Frames.Count > 10000 ||
		!state.Numbers.Pending.valid() || state.Order.Phase > legacyCanonicalKeyOrderExtract {
		return nil, ErrIntegrity
	}
	encoding := &LegacyCanonicalEncoding{initialized: true, eagerNumbers: state.EagerNumbers, order: state.Order.value(),
		ordering: state.Ordering, keyWork: state.KeyWork, inspectWork: state.InspectWork}
	return &legacyCanonicalPreparedRestore{state: state, encoding: encoding, store: store, budget: budget}, nil
}

func (work *legacyCanonicalPreparedRestore) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Keys.Ref.Hash)+len(work.state.Frames.Ref.Hash)+len(work.state.Numbers.Pending.Ref.Hash)), 1)
	if work.encoding != nil {
		retained, err := work.encoding.RetainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	for _, reader := range []interface{ retainedBytes() (int64, error) }{work.keys, work.frames} {
		retained, err := reader.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.numbers != nil {
		retained, err := work.numbers.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.fields != nil {
		retained, err := work.fields.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.value != nil {
		retained, err := work.value.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	work.frame.fields.retained(&heap)
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedRestore) room() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained+legacyNativeAnyCheckpointScratch > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyCanonicalPreparedRestore) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.encoding == nil {
		return false, ErrConflict
	}
	if work.ready {
		return true, nil
	}
	room, err := work.room()
	if err != nil {
		return false, err
	}
	if work.phase == 0 {
		if work.numbers == nil {
			work.numbers, err = newLegacyCanonicalPreparedNumbersRestore(ctx, work.state.Numbers, work.state.Binding, work.store, room)
			return false, err
		}
		ready, err := work.numbers.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		numbers, heap, err := work.numbers.take(ctx)
		work.numbers = nil
		if err != nil {
			return false, err
		}
		var metadata docRangeHeaderHeapEstimate
		numbers.retained(&metadata)
		if metadata.err != nil || heap.err != nil || heap.bytes < metadata.bytes {
			return false, ErrIntegrity
		}
		work.encoding.numbers = numbers
		work.encoding.rootHeap.include(docRangeHeaderHeapEstimate{bytes: heap.bytes - metadata.bytes})
		work.phase++
		return false, work.encoding.rootHeap.err
	}
	if work.phase == 1 {
		if work.keys == nil {
			work.keys, err = newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedKeysState](work.state.Keys, work.store)
			return false, err
		}
		if work.fields == nil {
			state, found, err := work.keys.next(ctx)
			if err != nil || !found {
				if err == nil {
					work.phase++
				}
				return false, err
			}
			index := work.encoding.mapKeys.length()
			if !state.Fields.valid() || state.EndKeys < 0 || uint64(state.EndKeys) > work.state.Keys.Count ||
				state.EndKeys != 0 && state.EndKeys <= index || state.EndKeys == 0 && (state.Fields.Count != 0 || state.Sorted) {
				return false, ErrIntegrity
			}
			reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](state.Fields, work.store)
			if err != nil {
				return false, err
			}
			work.fields = &legacyCanonicalPreparedFieldsRestore{reader: reader, numbers: &work.encoding.numbers,
				binding: work.state.Binding, store: work.store, budget: room}
			work.keyState = state
			return false, nil
		}
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		fields, payload := work.fields.fields, work.fields.payload
		work.fields.fields, work.fields.payload = legacyCanonicalPages[legacyCanonicalKeyField]{}, docRangeHeaderHeapEstimate{}
		work.fields.close()
		work.fields = nil
		bytes, err := fields.ownedBytes()
		if err != nil {
			return false, err
		}
		work.encoding.rootHeap.include(payload)
		work.encoding.pendingKeysHeap.include(docRangeHeaderHeapEstimate{bytes: bytes})
		if work.encoding.rootHeap.err != nil || work.encoding.pendingKeysHeap.err != nil {
			return false, ErrDocumentResourceLimit
		}
		_, forecast, err := work.encoding.mapKeys.appendRequirements()
		if err != nil {
			return false, err
		}
		before, err := work.encoding.mapKeys.ownedBytes()
		if err != nil {
			return false, err
		}
		retained, err := work.retainedBytes()
		if err != nil || retained+forecast.bytes-before+legacyNativeAnyCheckpointScratch > work.budget {
			return false, ErrDocumentResourceLimit
		}
		entry := legacyCanonicalEncodingKeys{fields: fields, heap: bytes, endKeys: work.keyState.EndKeys, sorted: work.keyState.Sorted}
		return false, work.encoding.mapKeys.append(entry)
	}
	if work.phase == 2 {
		if work.frames == nil {
			work.frames, err = newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFrameState](work.state.Frames, work.store)
			return false, err
		}
		return false, work.restoreFrame(ctx, room)
	}
	if uint64(work.encoding.mapKeys.length()) != work.state.Keys.Count || uint64(work.encoding.frames.length()) != work.state.Frames.Count {
		return false, ErrIntegrity
	}
	if work.encoding.ordering {
		frame := work.encoding.frames.at(work.encoding.frames.length() - 1)
		if frame.kind != 'o' || frame.ordered || !work.encoding.order.valid(&frame.fields) {
			return false, ErrIntegrity
		}
	}
	work.ready = true
	return true, nil
}

func (work *legacyCanonicalPreparedRestore) restoreFrame(ctx context.Context, room int64) error {
	if work.subphase == 0 {
		state, found, err := work.frames.next(ctx)
		if err != nil || !found {
			if err == nil {
				work.phase++
			}
			return err
		}
		if !state.Fields.valid() || state.KeysPosition < 0 || uint64(state.KeysPosition) > work.state.Keys.Count ||
			state.KeysEnd < 0 || uint64(state.KeysEnd) > work.state.Keys.Count || state.Position < 0 || state.Phase > 2 {
			return ErrIntegrity
		}
		reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](state.Fields, work.store)
		if err != nil {
			return err
		}
		work.frameData = state
		work.frame = legacyCanonicalEncodingFrame{keysPosition: state.KeysPosition, keysEnd: state.KeysEnd,
			position: state.Position, phase: state.Phase, kind: state.Kind, ordered: state.Ordered}
		work.fields = &legacyCanonicalPreparedFieldsRestore{reader: reader, numbers: &work.encoding.numbers,
			binding: work.state.Binding, store: work.store, budget: room}
		work.subphase++
		return nil
	}
	if work.subphase == 1 {
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.frame.fields = work.fields.fields
		work.encoding.rootHeap.include(work.fields.payload)
		work.fields.fields, work.fields.payload = legacyCanonicalPages[legacyCanonicalKeyField]{}, docRangeHeaderHeapEstimate{}
		work.fields.close()
		work.fields, work.subphase = nil, 2
		return work.encoding.rootHeap.err
	}
	if work.subphase == 2 || work.subphase == 4 || work.subphase == 6 {
		ref := work.frameData.Value
		if work.subphase == 4 {
			ref = work.frameData.Text
		} else if work.subphase == 6 {
			ref = work.frameData.Block
		}
		if ref == (legacyNativeCheckpointRef{}) {
			work.subphase += 2
			return nil
		}
		var err error
		work.value, err = newLegacyCanonicalPreparedValueRestore(ctx, ref, &work.encoding.numbers, work.state.Binding, work.store, room)
		if err == nil {
			work.subphase++
		}
		return err
	}
	if work.subphase == 3 || work.subphase == 5 || work.subphase == 7 {
		ready, err := work.value.step(ctx)
		if err != nil || !ready {
			return err
		}
		value, payload, err := work.value.take(ctx)
		work.value = nil
		if err != nil {
			return err
		}
		work.encoding.rootHeap.include(payload)
		switch work.subphase {
		case 3:
			work.frame.value = value
		case 5:
			switch text := value.(type) {
			case string:
				work.frame.text = text
			case json.Number:
				work.frame.text = string(text)
			case *json.Number:
				if text == nil {
					return ErrIntegrity
				}
				work.frame.text = string(*text)
			default:
				return ErrIntegrity
			}
		case 7:
			block, valid := value.(ContentBlock)
			if !valid {
				return ErrIntegrity
			}
			work.frame.block = block
		}
		work.subphase++
		return work.encoding.rootHeap.err
	}
	frame := &work.frame
	switch frame.kind {
	case 0:
		if !work.frameData.Value.valid() || frame.phase != 0 || frame.position != 0 {
			return ErrIntegrity
		}
	case 's', 'l':
		if !work.frameData.Text.valid() || frame.position > len(frame.text) || frame.phase > 1 {
			return ErrIntegrity
		}
	case 'o', 'r':
		if frame.position > frame.fields.length() || frame.keysEnd < frame.keysPosition {
			return ErrIntegrity
		}
	case 'b':
		if !work.frameData.Block.valid() || frame.position > 2 {
			return ErrIntegrity
		}
	case 'a', 'm', 'c', 'd', 'e':
		array, valid := frame.value.(*legacyCanonicalPreparedArray)
		if !valid || array == nil || frame.position > array.values.length() {
			return ErrIntegrity
		}
		frame.kind = 'e'
	default:
		return ErrIntegrity
	}
	_, forecast, err := work.encoding.frames.appendRequirements()
	if err != nil {
		return err
	}
	before, err := work.encoding.frames.ownedBytes()
	if err != nil {
		return err
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+forecast.bytes-before+legacyNativeAnyCheckpointScratch > work.budget {
		return ErrDocumentResourceLimit
	}
	if err := work.encoding.frames.append(*frame); err != nil {
		return err
	}
	work.frame, work.frameData, work.subphase = legacyCanonicalEncodingFrame{}, legacyCanonicalPreparedFrameState{}, 0
	return nil
}

func (work *legacyCanonicalPreparedRestore) take(ctx context.Context) (*LegacyCanonicalEncoding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil || !work.ready || work.encoding == nil {
		return nil, ErrDocRangeIndexPending
	}
	encoding := work.encoding
	work.encoding = nil
	work.close()
	return encoding, nil
}

func (work *legacyCanonicalPreparedRestore) close() {
	if work != nil {
		if work.encoding != nil {
			work.encoding.release()
		}
		if work.numbers != nil {
			work.numbers.close()
		}
		if work.fields != nil {
			work.fields.close()
		}
		if work.value != nil {
			work.value.close()
		}
		if work.keys != nil {
			work.keys.close()
		}
		if work.frames != nil {
			work.frames.close()
		}
		*work = legacyCanonicalPreparedRestore{}
	}
}
