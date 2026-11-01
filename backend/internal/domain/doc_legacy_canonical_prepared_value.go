package domain

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"unsafe"
)

const legacyCanonicalPreparedValueKind = "legacy-canonical-prepared-value"

const (
	legacyCanonicalPreparedNil uint8 = iota
	legacyCanonicalPreparedBool
	legacyCanonicalPreparedInt
	legacyCanonicalPreparedFloat
	legacyCanonicalPreparedString
	legacyCanonicalPreparedNumber
	legacyCanonicalPreparedNumberPointer
	legacyCanonicalPreparedObjectTag
	legacyCanonicalPreparedReplacementTag
	legacyCanonicalPreparedArrayTag
	legacyCanonicalPreparedBlock
)

type legacyCanonicalPreparedObject struct {
	count int
}

type legacyCanonicalPreparedReplacement struct {
	count int
}

type legacyCanonicalPreparedArray struct {
	values legacyCanonicalPages[any]
}

type legacyCanonicalPreparedArraySlice struct {
	value    any
	position int
}

type legacyCanonicalPreparedValueRecord struct {
	Kind     string                        `json:"kind"`
	Version  uint8                         `json:"version"`
	Binding  ContentHash                   `json:"binding"`
	Tag      uint8                         `json:"tag"`
	Bits     uint64                        `json:"bits"`
	Count    int                           `json:"count"`
	Number   int                           `json:"number"`
	Texts    [2]legacyNativeCheckpointText `json:"texts"`
	Children legacyNativeCheckpointTree    `json:"children"`
}

func (record legacyCanonicalPreparedValueRecord) validate(binding ContentHash) error {
	if record.Kind != legacyCanonicalPreparedValueKind || record.Version != legacyNativeCheckpointVersion || record.Binding != binding ||
		record.Tag > legacyCanonicalPreparedBlock || record.Count < 0 || record.Number < 0 {
		return ErrIntegrity
	}
	if record.Tag != legacyCanonicalPreparedBool && record.Tag != legacyCanonicalPreparedInt && record.Tag != legacyCanonicalPreparedFloat && record.Bits != 0 ||
		record.Tag != legacyCanonicalPreparedObjectTag && record.Tag != legacyCanonicalPreparedReplacementTag && record.Tag != legacyCanonicalPreparedArrayTag && record.Count != 0 ||
		record.Tag != legacyCanonicalPreparedNumber && record.Tag != legacyCanonicalPreparedNumberPointer && record.Number != 0 {
		return ErrIntegrity
	}
	if record.Tag == legacyCanonicalPreparedBool && record.Bits > 1 ||
		record.Tag == legacyCanonicalPreparedInt && int64(int(int64(record.Bits))) != int64(record.Bits) {
		return ErrIntegrity
	}
	if record.Tag == legacyCanonicalPreparedFloat {
		value := math.Float64frombits(record.Bits)
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return ErrIntegrity
		}
	}
	for position, text := range record.Texts {
		used := record.Tag == legacyCanonicalPreparedBlock || record.Tag == legacyCanonicalPreparedString && position == 0
		if !used && text != (legacyNativeCheckpointText{}) || text.Bytes < 0 || uint64(text.Bytes) > legacyNativeValueCheckpointPayloadLimit() ||
			!text.Tree.valid() || text.Tree.Count != uint64(text.Bytes) || (text.Bytes == 0) != (text.Tree == (legacyNativeCheckpointTree{})) {
			return ErrIntegrity
		}
	}
	if record.Tag == legacyCanonicalPreparedArrayTag {
		if !record.Children.valid() || record.Children.Count != uint64(record.Count) ||
			(record.Count == 0) != (record.Children == (legacyNativeCheckpointTree{})) {
			return ErrIntegrity
		}
	} else if record.Children != (legacyNativeCheckpointTree{}) {
		return ErrIntegrity
	}
	return nil
}

func legacyCanonicalPreparedValueBudget(retained, extra, budget int64) error {
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.include(docRangeHeaderHeapEstimate{bytes: extra})
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func legacyCanonicalPreparedValueWorkspace() docRangeHeaderHeapEstimate {
	heap := legacyNativeCheckpointStringValuesWorkspace()
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueRecord{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueWriteFrame{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueRestoreFrame{})))
	return heap
}

const (
	legacyCanonicalPreparedWriteEmit uint8 = iota
	legacyCanonicalPreparedWriteTextStart
	legacyCanonicalPreparedWriteText
	legacyCanonicalPreparedWriteChild
	legacyCanonicalPreparedWriteWaiting
	legacyCanonicalPreparedWriteAppend
	legacyCanonicalPreparedWriteFinish
)

type legacyCanonicalPreparedValueWriteFrame struct {
	parent    *legacyCanonicalPreparedValueWriteFrame
	record    legacyCanonicalPreparedValueRecord
	values    []any
	maps      []map[string]any
	blocks    []ContentBlock
	native    *legacyNativeBlockValues
	prepared  *legacyCanonicalPreparedArray
	texts     [2]string
	list      *legacyNativeCheckpointListWriter[legacyNativeCheckpointRef]
	pending   legacyNativeCheckpointRef
	position  int
	skip      int
	textSlot  int
	workspace int64
	phase     uint8
}

type legacyCanonicalPreparedValueWrite struct {
	frame     *legacyCanonicalPreparedValueWriteFrame
	text      *legacyNativeCheckpointTextWriter
	numbers   *legacyCanonicalNumberValidation
	binding   ContentHash
	store     legacyNativeCheckpointIO
	result    legacyNativeCheckpointRef
	workspace int64
	budget    int64
	ready     bool
	closed    bool
	failure   error
}

func newLegacyCanonicalPreparedValueWrite(ctx context.Context, value any, numbers *legacyCanonicalNumberValidation, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedValueWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(binding) != nil || store.Put == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueWrite{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedValueWriteFrame{})))
	planned.include(legacyCanonicalPreparedValueWorkspace())
	if planned.err != nil {
		return nil, planned.err
	}
	if err := legacyCanonicalPreparedValueBudget(0, planned.bytes, budget); err != nil {
		return nil, err
	}
	work := &legacyCanonicalPreparedValueWrite{numbers: numbers, binding: binding, store: store, budget: budget}
	if err := work.push(value); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalPreparedValueWrite) push(input any) error {
	frame := &legacyCanonicalPreparedValueWriteFrame{parent: work.frame}
	frame.record = legacyCanonicalPreparedValueRecord{Kind: legacyCanonicalPreparedValueKind, Version: legacyNativeCheckpointVersion, Binding: work.binding}
	if sliced, ok := input.(legacyCanonicalPreparedArraySlice); ok {
		length, err := legacyCanonicalPreparedArrayLength(sliced.value)
		if err != nil {
			return err
		}
		if sliced.position < 0 || sliced.position > length {
			return ErrIntegrity
		}
		input, frame.skip = sliced.value, sliced.position
	}
	if input != nil {
		value := reflect.ValueOf(input)
		if value.Kind() == reflect.Ptr && value.IsNil() {
			input = nil
		}
	}
	switch value := input.(type) {
	case nil:
	case bool:
		frame.record.Tag = legacyCanonicalPreparedBool
		if value {
			frame.record.Bits = 1
		}
	case int:
		frame.record.Tag, frame.record.Bits = legacyCanonicalPreparedInt, uint64(int64(value))
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return ErrIntegrity
		}
		frame.record.Tag, frame.record.Bits = legacyCanonicalPreparedFloat, math.Float64bits(value)
	case string:
		frame.record.Tag, frame.texts[0], frame.phase = legacyCanonicalPreparedString, value, legacyCanonicalPreparedWriteTextStart
	case EventKind, Role, ProviderKind, FidelityTier:
		frame.record.Tag, frame.texts[0], frame.phase = legacyCanonicalPreparedString, reflect.ValueOf(value).String(), legacyCanonicalPreparedWriteTextStart
	case json.Number, *json.Number:
		if work.numbers == nil {
			return ErrIntegrity
		}
		text, tag := "", legacyCanonicalPreparedNumber
		if number, ok := value.(json.Number); ok {
			text = string(number)
		} else {
			text, tag = string(*value.(*json.Number)), legacyCanonicalPreparedNumberPointer
		}
		index, err := work.numbers.index(text)
		if err != nil {
			return err
		}
		pending := work.numbers.pending.at(index)
		if pending == nil || legacyCanonicalNumberKey(*pending) != legacyCanonicalNumberKey(text) {
			return ErrIntegrity
		}
		frame.record.Tag, frame.record.Number = tag, index
	case map[string]any:
		if value != nil {
			frame.record.Tag, frame.record.Count = legacyCanonicalPreparedObjectTag, len(value)
		}
	case *legacyCanonicalReplacement:
		frame.record.Tag, frame.record.Count = legacyCanonicalPreparedReplacementTag, value.length()
	case *legacyCanonicalPreparedObject:
		frame.record.Tag, frame.record.Count = legacyCanonicalPreparedObjectTag, value.count
	case *legacyCanonicalPreparedReplacement:
		frame.record.Tag, frame.record.Count = legacyCanonicalPreparedReplacementTag, value.count
	case []any:
		if value != nil {
			frame.record.Tag, frame.record.Count, frame.values = legacyCanonicalPreparedArrayTag, len(value), value
		}
	case []map[string]any:
		if value != nil {
			frame.record.Tag, frame.record.Count, frame.maps = legacyCanonicalPreparedArrayTag, len(value), value
		}
	case []ContentBlock:
		if value != nil {
			frame.record.Tag, frame.record.Count, frame.blocks = legacyCanonicalPreparedArrayTag, len(value), value
		}
	case *legacyNativeBlockValues:
		frame.record.Tag, frame.record.Count, frame.native = legacyCanonicalPreparedArrayTag, value.length(), value
	case *legacyCanonicalPreparedArray:
		if !value.values.valid() {
			return ErrIntegrity
		}
		frame.record.Tag, frame.record.Count, frame.prepared = legacyCanonicalPreparedArrayTag, value.values.length(), value
	case ContentBlock:
		frame.record.Tag, frame.texts, frame.phase = legacyCanonicalPreparedBlock, [2]string{value.Text, value.Type}, legacyCanonicalPreparedWriteTextStart
	default:
		return ErrIntegrity
	}
	if frame.record.Count < 0 {
		return ErrIntegrity
	}
	if frame.record.Tag == legacyCanonicalPreparedArrayTag {
		frame.phase = legacyCanonicalPreparedWriteChild
		frame.list = &legacyNativeCheckpointListWriter[legacyNativeCheckpointRef]{}
	}
	work.frame = frame
	return work.refresh(frame)
}

func legacyCanonicalPreparedArrayLength(input any) (int, error) {
	switch value := input.(type) {
	case []any:
		return len(value), nil
	case []map[string]any:
		return len(value), nil
	case []ContentBlock:
		return len(value), nil
	case *legacyNativeBlockValues:
		if value == nil {
			return 0, nil
		}
		if value.length() >= 0 {
			return value.length(), nil
		}
	case *legacyCanonicalPreparedArray:
		if value == nil {
			return 0, nil
		}
		if value.values.valid() {
			return value.values.length(), nil
		}
	}
	return 0, ErrIntegrity
}

func (work *legacyCanonicalPreparedValueWrite) refresh(frame *legacyCanonicalPreparedValueWriteFrame) error {
	if frame == nil || frame.workspace > work.workspace {
		return ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*frame)))
	for _, text := range frame.record.Texts {
		heap.allocation(int64(len(text.Tree.Ref.Hash)), 1)
	}
	heap.allocation(int64(len(frame.record.Children.Ref.Hash)), 1)
	heap.allocation(int64(len(frame.pending.Hash)), 1)
	if frame.list != nil {
		retained, err := frame.list.retainedBytes()
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

func (frame *legacyCanonicalPreparedValueWriteFrame) child() (any, error) {
	if frame.position < 0 || frame.position >= frame.record.Count {
		return nil, ErrIntegrity
	}
	if frame.position < frame.skip {
		return nil, nil
	}
	switch {
	case frame.values != nil:
		return frame.values[frame.position], nil
	case frame.maps != nil:
		return frame.maps[frame.position], nil
	case frame.blocks != nil:
		return frame.blocks[frame.position], nil
	case frame.native != nil:
		value := frame.native.at(frame.position)
		if value != nil {
			return value.block, nil
		}
	case frame.prepared != nil:
		value := frame.prepared.values.at(frame.position)
		if value != nil {
			return *value, nil
		}
	}
	return nil, ErrIntegrity
}

func (work *legacyCanonicalPreparedValueWrite) advance(ctx context.Context) error {
	frame := work.frame
	if frame == nil {
		return ErrIntegrity
	}
	switch frame.phase {
	case legacyCanonicalPreparedWriteTextStart:
		limit := 1
		if frame.record.Tag == legacyCanonicalPreparedBlock {
			limit = 2
		}
		if frame.textSlot == limit {
			frame.phase = legacyCanonicalPreparedWriteEmit
			return nil
		}
		text := frame.texts[frame.textSlot]
		if len(text) <= LegacyCanonicalEncodingBytes {
			var tree legacyNativeCheckpointTree
			if len(text) > 0 {
				ref, err := legacyNativeCheckpointWrite(ctx, work.store, []byte(text))
				if err != nil {
					return err
				}
				tree = legacyNativeCheckpointTree{Ref: ref, Count: uint64(len(text))}
			}
			frame.record.Texts[frame.textSlot] = legacyNativeCheckpointText{Bytes: len(text), Tree: tree}
			frame.textSlot++
			if frame.textSlot == limit {
				frame.phase = legacyCanonicalPreparedWriteEmit
			}
			return work.refresh(frame)
		}
		read := func(current context.Context, offset, length int) ([]byte, error) {
			if err := current.Err(); err != nil {
				return nil, err
			}
			if offset < 0 || offset > len(text) || length <= 0 || length > min(LegacyCanonicalEncodingBytes, len(text)-offset) {
				return nil, ErrIntegrity
			}
			return []byte(text[offset : offset+length]), nil
		}
		var err error
		work.text, err = newLegacyNativeCheckpointTextWriter(ctx, len(text), read, work.store)
		if err == nil {
			frame.phase = legacyCanonicalPreparedWriteText
		}
		return err
	case legacyCanonicalPreparedWriteText:
		ready, err := work.text.step(ctx)
		if err != nil || !ready {
			return err
		}
		frame.record.Texts[frame.textSlot], err = work.text.take(ctx)
		if err != nil {
			return err
		}
		work.text = nil
		frame.textSlot++
		frame.phase = legacyCanonicalPreparedWriteTextStart
		return work.refresh(frame)
	case legacyCanonicalPreparedWriteChild:
		if frame.position == frame.record.Count {
			frame.phase = legacyCanonicalPreparedWriteFinish
			return nil
		}
		value, err := frame.child()
		if err != nil {
			return err
		}
		frame.phase = legacyCanonicalPreparedWriteWaiting
		return work.push(value)
	case legacyCanonicalPreparedWriteAppend:
		if !frame.pending.valid() || frame.list == nil {
			return ErrIntegrity
		}
		if err := frame.list.append(ctx, work.store, frame.pending); err != nil {
			return err
		}
		frame.pending = legacyNativeCheckpointRef{}
		frame.position++
		frame.phase = legacyCanonicalPreparedWriteChild
		return work.refresh(frame)
	case legacyCanonicalPreparedWriteFinish:
		var err error
		frame.record.Children, err = frame.list.finish(ctx, work.store)
		if err != nil {
			return err
		}
		frame.list.close()
		frame.list, frame.phase = nil, legacyCanonicalPreparedWriteEmit
		return work.refresh(frame)
	case legacyCanonicalPreparedWriteEmit:
		if err := frame.record.validate(work.binding); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, frame.record)
		if err != nil {
			return err
		}
		work.frame, work.workspace = frame.parent, work.workspace-frame.workspace
		if work.frame == nil {
			work.result, work.ready = ref, true
			return nil
		}
		if work.frame.phase != legacyCanonicalPreparedWriteWaiting {
			return ErrIntegrity
		}
		work.frame.pending, work.frame.phase = ref, legacyCanonicalPreparedWriteAppend
		return work.refresh(work.frame)
	default:
		return ErrIntegrity
	}
}

func (work *legacyCanonicalPreparedValueWrite) step(ctx context.Context) (bool, error) {
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
	retained, err := work.retainedBytes()
	if err == nil {
		scratch := legacyCanonicalPreparedValueWorkspace()
		err = scratch.err
		if err == nil {
			err = legacyCanonicalPreparedValueBudget(retained, scratch.bytes, work.budget)
		}
	}
	if err == nil {
		err = work.advance(ctx)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		retained, err = work.retainedBytes()
		if err == nil {
			err = legacyCanonicalPreparedValueBudget(retained, 0, work.budget)
		}
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyCanonicalPreparedValueWrite) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(docRangeHeaderHeapEstimate{bytes: work.workspace})
	heap.allocation(int64(len(work.result.Hash)), 1)
	if work.text != nil {
		retained, err := work.text.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		heap.allocation(1, 64)
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedValueWrite) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if !work.ready || work.frame != nil || work.text != nil || !work.result.valid() {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	retained, err := work.retainedBytes()
	if err == nil {
		err = legacyCanonicalPreparedValueBudget(retained, 0, work.budget)
	}
	if err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyCanonicalPreparedValueWrite) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalPreparedValueWrite) close() {
	if work == nil || work.closed {
		return
	}
	if work.text != nil {
		work.text.close()
	}
	for frame := work.frame; frame != nil; frame = frame.parent {
		if frame.list != nil {
			frame.list.close()
		}
	}
	failure := work.failure
	*work = legacyCanonicalPreparedValueWrite{closed: true, failure: failure}
}
