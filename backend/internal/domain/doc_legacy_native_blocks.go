package domain

import (
	"context"
	"errors"
	"reflect"
	"unsafe"
)

var legacyContentBlockNativeFields = newLegacyNativeObjectFields(reflect.TypeOf(ContentBlock{}))

const (
	legacyNativeBlocksStart uint8 = iota
	legacyNativeBlocksFirst
	legacyNativeBlocksValue
	legacyNativeBlocksScan
	legacyNativeBlocksApply
	legacyNativeBlocksNull
	legacyNativeBlocksAfter
	legacyNativeBlocksTail
	legacyNativeBlocksDone
)

type legacyNativeBlockList struct {
	mutation      *legacyNormalizationMutation
	decoded       []ContentBlock
	previous      []ContentBlock
	data          *legacyNativeBlockValues
	borrowed      docRangeHeaderHeapEstimate
	paged         bool
	payloads      []docRangeHeaderHeapEstimate
	previousHeaps []docRangeHeaderHeapEstimate
	excess        []int64
	previousExtra []int64
	estimate      docRangeHeaderHeapEstimate
	payload       docRangeHeaderHeapEstimate
	added         docRangeHeaderHeapEstimate
	visibleExcess int64
	totalExcess   int64
	block         ContentBlock
	object        *legacyNativeObject
	frame         *legacyJSONValueScanner
	position      int
	parentDepth   int
	end           int
	maximum       int
	count         int
	copied        int
	childStart    int
	literal       int
	seeded        int
	phase         uint8
	owned         bool
	previousOwned bool
	initialized   bool
	prefix        bool
	ownedFrame    bool
}

func newLegacyNativeBlockList(prior []ContentBlock, priorHeap docRangeHeaderHeapEstimate, from, to int) (*legacyNativeBlockList, error) {
	if from < 0 || to <= from || priorHeap.bytes < 0 {
		return nil, ErrIntegrity
	}
	if priorHeap.err != nil {
		return nil, priorHeap.err
	}
	if legacyContentBlockNativeFields.err != nil {
		return nil, legacyContentBlockNativeFields.err
	}
	if len(legacyContentBlockNativeFields.fields) != len(legacyNativeBlockValue{}.heaps) {
		return nil, ErrIntegrity
	}
	for _, field := range legacyContentBlockNativeFields.fields {
		if !field.scalarString {
			return nil, ErrIntegrity
		}
	}
	if cap(prior) > int(^uint(0)>>1)/len(legacyContentBlockNativeFields.fields) {
		return nil, ErrIntegrity
	}
	var minimum docRangeHeaderHeapEstimate
	minimum.allocation(1, int64(unsafe.Sizeof(prior)))
	minimum.include(legacyNativeBlocksBacking(cap(prior)))
	if minimum.err != nil {
		return nil, minimum.err
	}
	if priorHeap.bytes == 0 && cap(prior) == 0 {
		priorHeap = minimum
	}
	if priorHeap.bytes < minimum.bytes || cap(prior) == 0 && priorHeap.bytes != minimum.bytes {
		return nil, ErrIntegrity
	}
	list := &legacyNativeBlockList{
		decoded: prior, estimate: priorHeap, phase: legacyNativeBlocksDone, initialized: cap(prior) == 0,
		parentDepth: 1,
	}
	if err := list.start(from, to); err != nil {
		return nil, err
	}
	return list, nil
}

func (list *legacyNativeBlockList) start(from, to int) error {
	if list == nil || list.phase != legacyNativeBlocksDone || from < 0 || to <= from {
		return ErrIntegrity
	}
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return err
		}
		defer list.mutation.leave()
	}
	list.decoded = list.decoded[:cap(list.decoded)]
	list.position, list.end, list.maximum = from, to, (to-from)/3
	list.count, list.copied, list.childStart, list.literal = 0, 0, 0, 0
	if list.paged && list.data != nil {
		list.data.count = 0
	}
	list.visibleExcess = 0
	list.owned, list.prefix = false, false
	list.added = docRangeHeaderHeapEstimate{}
	list.phase = legacyNativeBlocksStart
	return nil
}

func legacyNativeBlocksBacking(capacity int) docRangeHeaderHeapEstimate {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(int64(capacity), int64(unsafe.Sizeof(ContentBlock{})))
	return estimate
}

func legacyNativeBlocksWidth() int {
	return int(unsafe.Sizeof(ContentBlock{})) + len(legacyContentBlockNativeFields.fields)*int(unsafe.Sizeof(docRangeHeaderHeapEstimate{})+unsafe.Sizeof(int64(0)))
}

func (list *legacyNativeBlockList) seed(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	fields := len(legacyContentBlockNativeFields.fields)
	width := legacyNativeBlocksWidth()
	if list.payloads == nil {
		list.payloads = make([]docRangeHeaderHeapEstimate, cap(list.decoded)*fields)
		list.excess = make([]int64, cap(list.decoded)*fields)
	}
	for list.seeded < len(list.decoded) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if ConversationManifestChunkBytes-input.workBytes < width || input.fields >= LegacyNormalizationStepEvents*fields {
			return false, nil
		}
		value := reflect.ValueOf(list.decoded[list.seeded])
		for index, field := range legacyContentBlockNativeFields.fields {
			var payload docRangeHeaderHeapEstimate
			payload.allocation(int64(value.Field(field.index).Len()), 1)
			list.payloads[list.seeded*fields+index] = payload
			list.payload.include(payload)
		}
		if list.payload.err != nil {
			return false, list.payload.err
		}
		input.workBytes += width
		input.fields++
		list.seeded++
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	estimate.include(legacyNativeBlocksBacking(cap(list.decoded)))
	estimate.include(list.payload)
	if estimate.err != nil {
		return false, estimate.err
	}
	if estimate.bytes != list.estimate.bytes {
		return false, ErrIntegrity
	}
	list.initialized = true
	return true, nil
}

func (list *legacyNativeBlockList) reserve(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	width := legacyNativeBlocksWidth()
	fields := len(legacyContentBlockNativeFields.fields)
	if list.paged {
		if list.data == nil || list.count >= list.maximum || list.count > list.data.records.length() {
			return false, ErrIntegrity
		}
		if list.count == list.data.records.length() {
			storage, err := list.data.records.appendBytes()
			if err != nil {
				return false, err
			}
			work := storage + int(unsafe.Sizeof(legacyNativeBlockValue{}))
			if work > ConversationManifestChunkBytes-input.workBytes {
				return false, nil
			}
			if err := list.data.records.append(legacyNativeBlockValue{}); err != nil {
				return false, err
			}
			input.workBytes += work
			list.refreshPagedHeap()
		}
		return true, list.estimate.err
	}
	if list.previous == nil && list.count >= cap(list.decoded) {
		if list.count >= list.maximum {
			return false, ErrIntegrity
		}
		if ConversationManifestChunkBytes-input.workBytes < 2*width {
			return false, nil
		}
		capacity := list.maximum
		if cap(list.decoded) <= list.maximum/2 {
			capacity = max(list.count+1, 2*cap(list.decoded))
		}
		if capacity > int(^uint(0)>>1)/fields {
			return false, ErrIntegrity
		}
		oldBacking := legacyNativeBlocksBacking(cap(list.decoded))
		nextHeap := list.estimate
		if oldBacking.err != nil || nextHeap.bytes < oldBacking.bytes {
			return false, ErrIntegrity
		}
		nextHeap.bytes -= oldBacking.bytes
		nextHeap.include(legacyNativeBlocksBacking(capacity))
		if nextHeap.err != nil {
			return false, nextHeap.err
		}
		next := make([]ContentBlock, capacity)
		nextHeaps := make([]docRangeHeaderHeapEstimate, capacity*fields)
		nextExcess := make([]int64, capacity*fields)
		list.previous, list.previousOwned = list.decoded, list.owned
		list.previousHeaps, list.payloads = list.payloads, nextHeaps
		list.previousExtra, list.excess = list.excess, nextExcess
		list.decoded, list.owned, list.copied, list.estimate = next, true, 0, nextHeap
		if len(list.previous) == 0 {
			list.previous, list.previousHeaps, list.previousOwned = nil, nil, false
			list.previousExtra = nil
		}
	}
	for list.previous != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		length := min(len(list.previous)-list.copied, canonicalCheckBytes/(2*width), (ConversationManifestChunkBytes-input.workBytes)/(2*width))
		if length <= 0 {
			return false, nil
		}
		copy(list.decoded[list.copied:list.copied+length], list.previous[list.copied:list.copied+length])
		copy(list.payloads[list.copied*fields:(list.copied+length)*fields], list.previousHeaps[list.copied*fields:(list.copied+length)*fields])
		copy(list.excess[list.copied*fields:(list.copied+length)*fields], list.previousExtra[list.copied*fields:(list.copied+length)*fields])
		list.copied += length
		input.workBytes += 2 * width * length
		if list.copied == len(list.previous) {
			list.previous, list.previousOwned, list.previousHeaps = nil, false, nil
			list.previousExtra = nil
		}
	}
	return true, nil
}

func (list *legacyNativeBlockList) blockRecord() (legacyNativeBlockValue, error) {
	if list.paged {
		if list.data == nil {
			return legacyNativeBlockValue{}, ErrIntegrity
		}
		record := list.data.records.at(list.count)
		if record == nil {
			return legacyNativeBlockValue{}, ErrIntegrity
		}
		return *record, nil
	}
	fields := len(legacyContentBlockNativeFields.fields)
	if list.count < 0 || list.count >= len(list.decoded) || fields != len(legacyNativeBlockValue{}.heaps) ||
		len(list.payloads) < (list.count+1)*fields || len(list.excess) < (list.count+1)*fields {
		return legacyNativeBlockValue{}, ErrIntegrity
	}
	record := legacyNativeBlockValue{block: list.decoded[list.count]}
	copy(record.heaps[:], list.payloads[list.count*fields:(list.count+1)*fields])
	copy(record.excess[:], list.excess[list.count*fields:(list.count+1)*fields])
	return record, nil
}

func (list *legacyNativeBlockList) refreshPagedHeap() {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	if list.data != nil {
		list.data.payload = list.payload
	}
	heap.include(list.data.heap())
	list.estimate = heap
}

func (list *legacyNativeBlockList) beginBlock(prefix bool) error {
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return err
		}
		defer list.mutation.leave()
	}
	if prefix && list.parentDepth >= 10000 {
		return ErrIntegrity
	}
	record, err := list.blockRecord()
	if err != nil {
		return err
	}
	list.block = record.block
	object, err := newLegacyNativeObject(reflect.ValueOf(&list.block), &legacyContentBlockNativeFields, list.parentDepth+1, false)
	if err != nil {
		return err
	}
	if list.mutation != nil {
		object.mutation = list.mutation
	}
	fields := len(legacyContentBlockNativeFields.fields)
	if len(object.backingExcess) != fields {
		return ErrIntegrity
	}
	copy(object.backingExcess, record.excess[:])
	for index := range object.heaps {
		object.heaps[index].allocation(1, int64(unsafe.Sizeof(string(""))))
		object.heaps[index].include(record.heaps[index])
		if object.heaps[index].err != nil {
			return object.heaps[index].err
		}
	}
	end := list.position
	if prefix {
		end = list.end
	}
	if err := object.start(list.childStart, end); err != nil {
		return err
	}
	object.prefix = prefix
	list.object, list.phase = object, legacyNativeBlocksApply
	return nil
}

func (list *legacyNativeBlockList) applyBlock() error {
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return err
		}
		defer list.mutation.leave()
	}
	fields := len(legacyContentBlockNativeFields.fields)
	record, err := list.blockRecord()
	if err != nil {
		return err
	}
	excess, err := list.object.backingExcessBytes(nil)
	if err != nil {
		return err
	}
	visible := docRangeHeaderHeapEstimate{bytes: list.visibleExcess}
	visible.include(docRangeHeaderHeapEstimate{bytes: excess})
	if visible.err != nil || len(list.object.backingExcess) != fields {
		return ErrIntegrity
	}
	total := docRangeHeaderHeapEstimate{bytes: list.totalExcess}
	for _, previous := range record.excess {
		if previous < 0 || total.bytes < previous {
			return ErrIntegrity
		}
		total.bytes -= previous
	}
	total.include(docRangeHeaderHeapEstimate{bytes: excess})
	if total.err != nil {
		return total.err
	}
	var storage docRangeHeaderHeapEstimate
	storage.allocation(1, int64(unsafe.Sizeof(string(""))))
	prior, next := reflect.ValueOf(record.block), reflect.ValueOf(list.block)
	for index, field := range legacyContentBlockNativeFields.fields {
		old := record.heaps[index]
		payload := list.object.heaps[index]
		if payload.err != nil || payload.bytes < storage.bytes || old.err != nil ||
			list.estimate.bytes < old.bytes || list.payload.bytes < old.bytes {
			return ErrIntegrity
		}
		payload.bytes -= storage.bytes
		list.estimate.bytes -= old.bytes
		list.payload.bytes -= old.bytes
		list.estimate.include(payload)
		list.payload.include(payload)
		if unsafe.StringData(prior.Field(field.index).String()) != unsafe.StringData(next.Field(field.index).String()) {
			list.added.include(payload)
		}
		record.heaps[index] = payload
	}
	if list.estimate.err != nil || list.payload.err != nil || list.added.err != nil {
		return errors.Join(list.estimate.err, list.payload.err, list.added.err)
	}
	record.block = list.block
	copy(record.excess[:], list.object.backingExcess)
	if list.paged {
		*list.data.records.at(list.count) = record
		list.data.count = list.count + 1
		list.refreshPagedHeap()
	} else {
		list.decoded[list.count] = list.block
		copy(list.payloads[list.count*fields:(list.count+1)*fields], record.heaps[:])
		copy(list.excess[list.count*fields:(list.count+1)*fields], record.excess[:])
	}
	list.visibleExcess = visible.bytes
	list.totalExcess = total.bytes
	list.block, list.object = ContentBlock{}, nil
	list.count++
	list.phase = legacyNativeBlocksAfter
	return nil
}

func (list *legacyNativeBlockList) finishArray() {
	if list.paged {
		if list.count == 0 {
			*list.data = legacyNativeBlockValues{}
			list.payload, list.visibleExcess, list.totalExcess = docRangeHeaderHeapEstimate{}, 0, 0
		}
		list.data.count = list.count
		list.refreshPagedHeap()
		list.phase = legacyNativeBlocksTail
		return
	}
	if list.count == 0 {
		list.decoded, list.owned = make([]ContentBlock, 0), false
		list.payloads, list.payload = nil, docRangeHeaderHeapEstimate{}
		list.excess, list.visibleExcess = nil, 0
		list.totalExcess = 0
		list.estimate = docRangeHeaderHeapEstimate{}
		list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	} else {
		list.decoded = list.decoded[:list.count]
	}
	list.phase = legacyNativeBlocksTail
}

func (list *legacyNativeBlockList) step(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list == nil || input == nil || list.end > input.source.Bytes || input.unbounded || list.parentDepth < 1 || list.parentDepth > 10000 {
		return false, ErrIntegrity
	}
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return false, err
		}
		defer list.mutation.leave()
	}
	if !list.initialized {
		ready, err := list.seed(ctx, input)
		if err != nil || !ready {
			return false, err
		}
	}
	if list.phase == legacyNativeBlocksDone {
		return true, nil
	}
	fieldLimit := input.fieldActionLimit(LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields))
	if input.eventsRunning != nil {
		fieldLimit = min(fieldLimit, input.fields+LegacyNormalizationStepEvents*len(legacyContentBlockNativeFields.fields))
		previousLimit := input.eventsFieldLimit
		input.eventsFieldLimit = fieldLimit
		defer func() { input.eventsFieldLimit = previousLimit }()
	}
	for input.workBytes < ConversationManifestChunkBytes && input.fields < fieldLimit {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if list.phase == legacyNativeBlocksDone {
			return true, nil
		}
		if list.phase == legacyNativeBlocksApply {
			if list.frame == nil {
				list.frame, list.ownedFrame = &legacyJSONValueScanner{}, true
			}
			complete, err := list.object.step(ctx, input, list.frame)
			if err != nil || !complete {
				return false, err
			}
			list.position = list.object.position
			width := legacyNativeBlocksWidth()
			if ConversationManifestChunkBytes-input.workBytes < width {
				return false, nil
			}
			if err := list.applyBlock(); err != nil {
				return false, err
			}
			input.workBytes += width
			input.fields++
			continue
		}
		if list.phase == legacyNativeBlocksTail && (list.prefix || list.position == list.end) {
			list.phase = legacyNativeBlocksDone
			list.added, list.owned = docRangeHeaderHeapEstimate{}, false
			return true, nil
		}
		if list.position >= list.end {
			return false, ErrIntegrity
		}
		current, err := input.current(list.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		input.semanticBytes++
		switch list.phase {
		case legacyNativeBlocksStart:
			switch {
			case legacyNormalizationSpace(current):
				list.position++
			case current == '[':
				if list.paged && list.data == nil {
					storage := int(unsafe.Sizeof(legacyNativeBlockValues{}))
					if storage > ConversationManifestChunkBytes-input.workBytes {
						return false, nil
					}
					list.data = &legacyNativeBlockValues{}
					input.workBytes += storage
					list.refreshPagedHeap()
				}
				list.position++
				list.phase = legacyNativeBlocksFirst
			case current == 'n':
				list.literal, list.phase = 0, legacyNativeBlocksNull
			default:
				return false, ErrIntegrity
			}
		case legacyNativeBlocksFirst, legacyNativeBlocksValue:
			if legacyNormalizationSpace(current) {
				list.position++
				continue
			}
			if current == ']' && list.phase == legacyNativeBlocksFirst {
				list.position++
				list.finishArray()
				continue
			}
			if current != '{' && current != 'n' {
				return false, legacyNativeFieldTypeError(current, legacyContentBlockNativeFields.model, list.position)
			}
			ready, err := list.reserve(ctx, input)
			if err != nil || !ready {
				return false, err
			}
			list.childStart = list.position
			if current == '{' {
				if err := list.beginBlock(true); err != nil {
					return false, err
				}
				continue
			}
			list.literal = 0
			list.phase = legacyNativeBlocksScan
		case legacyNativeBlocksScan:
			if current != "null"[list.literal] {
				return false, ErrIntegrity
			}
			list.position++
			list.literal++
			if list.literal == len("null") {
				if err := list.beginBlock(false); err != nil {
					return false, err
				}
			}
		case legacyNativeBlocksNull:
			if current != "null"[list.literal] {
				return false, ErrIntegrity
			}
			list.position++
			list.literal++
			if list.literal == len("null") {
				list.decoded, list.payloads = nil, nil
				list.data = nil
				list.excess, list.visibleExcess = nil, 0
				list.totalExcess = 0
				list.payload, list.estimate = docRangeHeaderHeapEstimate{}, docRangeHeaderHeapEstimate{}
				list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
				list.phase = legacyNativeBlocksTail
			}
		case legacyNativeBlocksAfter:
			switch {
			case legacyNormalizationSpace(current):
				list.position++
			case current == ',':
				list.position++
				list.phase = legacyNativeBlocksValue
			case current == ']':
				list.position++
				list.finishArray()
			default:
				return false, ErrIntegrity
			}
		case legacyNativeBlocksTail:
			if !legacyNormalizationSpace(current) {
				return false, ErrIntegrity
			}
			list.position++
		default:
			return false, ErrIntegrity
		}
	}
	return false, ctx.Err()
}

func (list *legacyNativeBlockList) values() []ContentBlock {
	return list.decoded
}

func (list *legacyNativeBlockList) heap() docRangeHeaderHeapEstimate {
	return list.estimate
}

func (list *legacyNativeBlockList) backingExcess() (int64, error) {
	if list == nil || list.phase != legacyNativeBlocksDone || list.visibleExcess < 0 {
		return 0, ErrIntegrity
	}
	return list.visibleExcess, nil
}

func (list *legacyNativeBlockList) retained(estimate *docRangeHeaderHeapEstimate) {
	if list == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*list)))
	if list.ownedFrame && list.frame != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*list.frame)))
	}
	if list.paged {
		if list.estimate.err != nil || list.borrowed.err != nil || list.estimate.bytes < 0 || list.borrowed.bytes < 0 {
			estimate.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
			return
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: max(int64(0), list.estimate.bytes-list.borrowed.bytes)})
	} else {
		estimate.allocation(int64(cap(list.payloads)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
		estimate.allocation(int64(cap(list.previousHeaps)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
		estimate.allocation(int64(cap(list.excess)), int64(unsafe.Sizeof(int64(0))))
		estimate.allocation(int64(cap(list.previousExtra)), int64(unsafe.Sizeof(int64(0))))
		estimate.include(list.added)
		if list.owned {
			estimate.include(legacyNativeBlocksBacking(cap(list.decoded)))
		}
		if list.previousOwned {
			estimate.include(legacyNativeBlocksBacking(cap(list.previous)))
		}
	}
	if list.object != nil {
		bytes, err := list.object.retained()
		scratch := docRangeHeaderHeapEstimate{bytes: bytes, err: err}
		var storage docRangeHeaderHeapEstimate
		storage.allocation(1, int64(unsafe.Sizeof(string(""))))
		record, err := list.blockRecord()
		if err != nil {
			estimate.include(docRangeHeaderHeapEstimate{err: err})
			return
		}
		prior, next := reflect.ValueOf(record.block), reflect.ValueOf(list.block)
		for index, field := range legacyContentBlockNativeFields.fields {
			heap := list.object.heaps[index]
			if heap.err != nil || heap.bytes < storage.bytes || scratch.bytes < heap.bytes {
				scratch.err = ErrIntegrity
				break
			}
			scratch.bytes -= heap.bytes
			if unsafe.StringData(prior.Field(field.index).String()) != unsafe.StringData(next.Field(field.index).String()) {
				heap.bytes -= storage.bytes
				scratch.include(heap)
			}
		}
		estimate.include(scratch)
	}
}
