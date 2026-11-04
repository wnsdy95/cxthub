package domain

import (
	"context"
	"errors"
	"reflect"
	"unsafe"
)

const (
	legacyNativeEventsStart uint8 = iota
	legacyNativeEventsFirst
	legacyNativeEventsValue
	legacyNativeEventsScan
	legacyNativeEventsApply
	legacyNativeEventsNull
	legacyNativeEventsAfter
	legacyNativeEventsTail
	legacyNativeEventsDone
)

type legacyNativeEventRecord struct {
	heap   docRangeHeaderHeapEstimate
	excess int64
}

type legacyNativeEventList struct {
	mutation      *legacyNormalizationMutation
	decoded       []CIREvent
	previous      []CIREvent
	values        *legacyNativeEventValues
	borrowed      docRangeHeaderHeapEstimate
	paged         bool
	records       legacyCanonicalPages[legacyNativeEventRecord]
	payload       docRangeHeaderHeapEstimate
	added         docRangeHeaderHeapEstimate
	estimate      docRangeHeaderHeapEstimate
	excess        int64
	child         wire
	object        *legacyNativeObject
	frame         *legacyJSONValueScanner
	parent        *legacyNativeEventList
	cursor        *legacyNativeEventList
	position      int
	end           int
	parentDepth   int
	maximum       int
	count         int
	copied        int
	childStart    int
	literal       int
	phase         uint8
	first         byte
	prefix        bool
	owned         bool
	previousOwned bool
}

func newLegacyNativeEventList(prior []CIREvent, heap docRangeHeaderHeapEstimate, from, to int) (*legacyNativeEventList, error) {
	if cap(prior) != 0 || heap.err != nil || heap.bytes < 0 {
		return nil, ErrIntegrity
	}
	list := &legacyNativeEventList{decoded: prior, parentDepth: 1, phase: legacyNativeEventsDone}
	list.refreshHeap()
	if heap.bytes != 0 && heap.bytes != list.estimate.bytes {
		return nil, ErrIntegrity
	}
	if err := list.start(from, to); err != nil {
		return nil, err
	}
	return list, nil
}

func legacyNativeEventsBacking(capacity int) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(capacity), int64(unsafe.Sizeof(CIREvent{})))
	return heap
}

func (list *legacyNativeEventList) refreshHeap() {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	if list.paged {
		heap.include(list.values.heap())
	} else {
		heap.include(legacyNativeEventsBacking(cap(list.decoded)))
		heap.include(list.payload)
	}
	list.estimate = heap
}

func (list *legacyNativeEventList) start(from, to int) error {
	if list == nil || list.phase != legacyNativeEventsDone || from < 0 || to <= from || list.previous != nil ||
		list.records.length() > cap(list.decoded) || list.estimate.err != nil {
		return ErrIntegrity
	}
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return err
		}
		defer list.mutation.leave()
	}
	list.decoded = list.decoded[:cap(list.decoded)]
	list.position, list.end, list.maximum = from, to, (to-from)/2
	list.count, list.copied, list.literal, list.childStart = 0, 0, 0, 0
	list.owned, list.previousOwned, list.prefix = false, false, false
	list.parent, list.cursor = nil, nil
	if list.paged && list.values != nil {
		list.values.count = 0
	}
	list.added = docRangeHeaderHeapEstimate{}
	list.phase = legacyNativeEventsStart
	return nil
}

func (list *legacyNativeEventList) reserve(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	width := int(unsafe.Sizeof(CIREvent{}))
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list.paged {
		if list.values == nil || list.count >= list.maximum || list.count > list.values.records.length() {
			return false, ErrIntegrity
		}
		if list.count == list.values.records.length() {
			storage, err := list.values.records.appendBytes()
			if err != nil {
				return false, err
			}
			work := storage + int(unsafe.Sizeof(legacyNativeEventValue{}))
			if work > ConversationManifestChunkBytes-input.workBytes {
				return false, nil
			}
			if err := list.values.records.append(legacyNativeEventValue{}); err != nil {
				return false, err
			}
			input.workBytes += work
			list.refreshHeap()
		}
		return true, list.estimate.err
	}
	if list.previous == nil && list.count == cap(list.decoded) {
		if list.count >= list.maximum || 2*width > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		growth := max(1, cap(list.decoded))
		if cap(list.decoded) >= 256 {
			growth = cap(list.decoded) / 2
		}
		capacity := cap(list.decoded) + min(growth, list.maximum-cap(list.decoded))
		list.previous, list.previousOwned = list.decoded, list.owned
		list.decoded, list.owned, list.copied = make([]CIREvent, capacity), true, 0
		input.workBytes += 2 * width
		list.refreshHeap()
		if list.estimate.err != nil {
			return false, list.estimate.err
		}
		if len(list.previous) == 0 {
			list.previous, list.previousOwned = nil, false
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
		list.copied += length
		input.workBytes += length * 2 * width
		if list.copied == len(list.previous) {
			list.previous, list.previousOwned = nil, false
		}
	}
	return true, nil
}

func (list *legacyNativeEventList) beginChild(prefix bool) error {
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return err
		}
		defer list.mutation.leave()
	}
	if list.parentDepth >= 10000 && prefix {
		return ErrIntegrity
	}
	list.child = wire{}
	if list.object == nil {
		object, err := newLegacyNativeObject(reflect.ValueOf(&list.child), &legacyEventNativeFields, list.parentDepth+1, true)
		if err != nil {
			return err
		}
		list.object = object
	} else {
		if err := list.object.releaseValues(); err != nil {
			return err
		}
		list.object.parentDepth = list.parentDepth + 1
	}
	if list.mutation != nil {
		list.object.mutation = list.mutation
	}
	list.object.pagedEvents = list.paged
	end := list.position
	if prefix {
		end = list.end
	}
	if err := list.object.start(list.childStart, end); err != nil {
		return err
	}
	list.object.prefix = prefix
	list.phase = legacyNativeEventsApply
	return nil
}

func (list *legacyNativeEventList) commitChild(input *legacyNormalizationInput) (bool, error) {
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return false, err
		}
		defer list.mutation.leave()
	}
	storageBytes := 0
	if !list.paged && list.count == list.records.length() {
		var err error
		storageBytes, err = list.records.appendBytes()
		if err != nil {
			return false, err
		}
	}
	width := int(unsafe.Sizeof(CIREvent{})) + int(unsafe.Sizeof(legacyNativeEventRecord{}))
	if list.paged {
		width = int(unsafe.Sizeof(CIREvent{})) + int(unsafe.Sizeof(legacyNativeEventValue{}))
	}
	if storageBytes+width > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	var payload docRangeHeaderHeapEstimate
	for index, field := range legacyEventNativeFields.fields {
		heap := list.object.heaps[index]
		if heap.err != nil {
			return false, heap.err
		}
		if heap.bytes == 0 {
			if !reflect.ValueOf(list.child).Field(field.index).IsZero() {
				return false, ErrIntegrity
			}
			continue
		}
		var storage docRangeHeaderHeapEstimate
		storage.allocation(1, int64(legacyEventNativeFields.model.Field(field.index).Type.Size()))
		if storage.err != nil || heap.bytes < storage.bytes {
			return false, ErrIntegrity
		}
		heap.bytes -= storage.bytes
		payload.include(heap)
	}
	if payload.err != nil {
		return false, payload.err
	}
	extra, err := list.object.backingExcessBytes(nil)
	if err != nil {
		return false, err
	}
	for _, blocks := range list.object.blocks {
		if blocks != nil {
			if blocks.totalExcess < blocks.visibleExcess || extra > int64(^uint64(0)>>1)-(blocks.totalExcess-blocks.visibleExcess) {
				return false, ErrIntegrity
			}
			extra += blocks.totalExcess - blocks.visibleExcess
		}
	}
	var prior legacyNativeEventRecord
	if list.paged {
		if list.values == nil || list.count >= list.values.records.length() {
			return false, ErrIntegrity
		}
		record := list.values.records.at(list.count)
		prior = legacyNativeEventRecord{heap: record.heap, excess: record.excess}
	} else if list.count < list.records.length() {
		prior = *list.records.at(list.count)
	} else if err := list.records.append(legacyNativeEventRecord{}); err != nil {
		return false, err
	}
	ledger := &list.payload
	if list.paged {
		ledger = &list.values.payload
	}
	if prior.heap.err != nil || prior.heap.bytes > ledger.bytes || prior.excess > list.excess || extra < 0 {
		return false, ErrIntegrity
	}
	ledger.bytes -= prior.heap.bytes
	ledger.include(payload)
	if !list.paged {
		list.added.include(payload)
	}
	if ledger.err != nil || list.added.err != nil {
		return false, ErrIntegrity
	}
	excess := docRangeHeaderHeapEstimate{bytes: list.excess - prior.excess}
	excess.include(docRangeHeaderHeapEstimate{bytes: extra})
	if excess.err != nil {
		return false, excess.err
	}
	list.excess = excess.bytes
	event := CIREvent(list.child)
	event.fieldPresence = list.object.presence
	if list.paged {
		replacement, err := list.object.pagedReplacement()
		if err != nil {
			return false, err
		}
		blocks, err := list.object.pagedBlocks()
		if err != nil {
			return false, err
		}
		*list.values.records.at(list.count) = legacyNativeEventValue{event: event, replacement: replacement, blocks: blocks, heap: payload, excess: extra}
	} else {
		*list.records.at(list.count) = legacyNativeEventRecord{heap: payload, excess: extra}
		list.decoded[list.count] = event
	}
	list.position = list.object.position
	list.count++
	if list.paged {
		list.values.count = list.count
	}
	list.child = wire{}
	if err := list.object.releaseValues(); err != nil {
		return false, err
	}
	list.phase = legacyNativeEventsAfter
	input.workBytes += storageBytes + width
	input.fields++
	list.refreshHeap()
	return true, list.estimate.err
}

func (list *legacyNativeEventList) finish() {
	if list.paged {
		if list.count == 0 {
			*list.values = legacyNativeEventValues{}
			list.excess = 0
		}
	} else if list.count == 0 {
		list.decoded = []CIREvent{}
		list.records.clear()
		list.payload = docRangeHeaderHeapEstimate{}
		list.excess = 0
	} else {
		list.decoded = list.decoded[:list.count]
	}
	list.refreshHeap()
	list.phase = legacyNativeEventsTail
	if list.prefix {
		list.phase = legacyNativeEventsDone
	}
}

func (list *legacyNativeEventList) advance(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if list.parentDepth < 1 || list.parentDepth > 10000 || list.position < 0 || list.position > list.end || list.end > input.source.Bytes {
		return false, ErrIntegrity
	}
	if list.phase == legacyNativeEventsDone {
		return true, nil
	}
	if list.phase == legacyNativeEventsApply {
		complete, err := list.object.step(ctx, input, list.frame)
		if err != nil || !complete {
			return false, err
		}
		advanced, err := list.commitChild(input)
		if err != nil || !advanced {
			if err == nil {
				err = errLegacyNormalizationYield
			}
			return false, err
		}
		return false, nil
	}
	if list.position == list.end {
		if list.phase == legacyNativeEventsTail {
			list.phase = legacyNativeEventsDone
			return true, nil
		}
		return false, ErrIntegrity
	}
	current, err := input.current(list.position)
	if errors.Is(err, errLegacyNormalizationYield) {
		return false, errLegacyNormalizationYield
	}
	if err != nil {
		return false, err
	}
	switch list.phase {
	case legacyNativeEventsStart:
		if current == '[' {
			if list.paged && list.values == nil {
				width := int(unsafe.Sizeof(legacyNativeEventValues{}))
				if width > ConversationManifestChunkBytes-input.workBytes {
					return false, errLegacyNormalizationYield
				}
				list.values = &legacyNativeEventValues{}
				input.workBytes += width
				list.refreshHeap()
			}
			list.position++
			list.phase = legacyNativeEventsFirst
		} else if current == 'n' {
			list.phase = legacyNativeEventsNull
		} else {
			return false, ErrIntegrity
		}
	case legacyNativeEventsFirst, legacyNativeEventsValue:
		if legacyNormalizationSpace(current) {
			list.position++
		} else if current == ']' && list.phase == legacyNativeEventsFirst {
			list.position++
			list.finish()
		} else {
			ready, err := list.reserve(ctx, input)
			if err != nil || !ready {
				if err == nil {
					err = errLegacyNormalizationYield
				}
				return false, err
			}
			list.childStart, list.first = list.position, current
			if current == '{' {
				if err := list.beginChild(true); err != nil {
					return false, err
				}
			} else {
				if err := list.frame.reset(list.parentDepth); err != nil {
					return false, err
				}
				list.phase = legacyNativeEventsScan
			}
		}
	case legacyNativeEventsScan:
		consumed, complete, err := list.frame.add(current)
		if err != nil || !consumed && !complete {
			return false, ErrIntegrity
		}
		if consumed {
			list.position++
		}
		if complete {
			if list.first != 'n' {
				return false, legacyNativeFieldTypeError(list.first, reflect.TypeFor[wire](), list.position-list.childStart)
			}
			if err := list.beginChild(false); err != nil {
				return false, err
			}
		}
	case legacyNativeEventsNull:
		if current != "null"[list.literal] {
			return false, ErrIntegrity
		}
		list.position++
		list.literal++
		if list.literal == len("null") {
			list.decoded = nil
			list.values = nil
			list.records.clear()
			list.payload = docRangeHeaderHeapEstimate{}
			list.excess = 0
			list.refreshHeap()
			list.phase = legacyNativeEventsTail
			if list.prefix {
				list.phase = legacyNativeEventsDone
			}
		}
	case legacyNativeEventsAfter:
		switch {
		case legacyNormalizationSpace(current):
			list.position++
		case current == ',':
			list.position++
			list.phase = legacyNativeEventsValue
		case current == ']':
			list.position++
			list.finish()
		default:
			return false, ErrIntegrity
		}
	case legacyNativeEventsTail:
		if !legacyNormalizationSpace(current) {
			return false, ErrIntegrity
		}
		list.position++
	default:
		return false, ErrIntegrity
	}
	return list.phase == legacyNativeEventsDone, nil
}

func (list *legacyNativeEventList) step(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list.mutation != nil {
		if err := list.mutation.enter(); err != nil {
			return false, err
		}
		defer list.mutation.leave()
	}
	if input.eventsRunning != nil || input.eventsPending != nil || list.frame == nil {
		return false, ErrIntegrity
	}
	current := list.cursor
	if current == nil {
		current = list
	}
	defer func() {
		input.eventsRunning, input.eventsPending = nil, nil
		if list.phase != legacyNativeEventsDone {
			list.cursor = current
		} else {
			list.cursor = nil
		}
	}()
	for input.workBytes < ConversationManifestChunkBytes && input.fields < input.fieldActionLimit(LegacyNormalizationStepEvents*len(legacyEventNativeFields.fields)) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		input.eventsRunning = current
		before := current.phase
		workBytes, fields, values := input.workBytes, input.fields, input.values
		complete, err := current.advance(ctx, input)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if pending := input.eventsPending; pending != nil {
			input.eventsPending = nil
			if pending == current || pending.parent != nil && pending.parent != current {
				return false, ErrIntegrity
			}
			pending.parent, current = current, pending
			continue
		}
		if complete {
			if current == list {
				list.cursor = nil
				return true, nil
			}
			if current.parent == nil {
				return false, ErrIntegrity
			}
			parent := current.parent
			current.parent, current = nil, parent
			continue
		}
		if before == legacyNativeEventsApply && current.phase == legacyNativeEventsApply &&
			input.workBytes == workBytes && input.fields == fields && input.values == values {
			return false, nil
		}
	}
	return false, ctx.Err()
}

func (list *legacyNativeEventList) progressPosition() int {
	position := list.position
	current := list.cursor
	if current == nil {
		current = list
	}
	position = max(position, current.position)
	if current.phase == legacyNativeEventsApply && current.object != nil {
		position = max(position, current.object.progressPosition())
	}
	return position
}

func (list *legacyNativeEventList) retained(heap *docRangeHeaderHeapEstimate) {
	for current := list; current != nil; {
		heap.allocation(1, int64(unsafe.Sizeof(*current)))
		if current.paged {
			if current.estimate.err != nil || current.borrowed.err != nil || current.borrowed.bytes < 0 {
				heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
				return
			}
			if current.estimate.bytes > current.borrowed.bytes {
				heap.include(docRangeHeaderHeapEstimate{bytes: current.estimate.bytes - current.borrowed.bytes})
			}
		} else {
			current.records.retained(heap)
			heap.include(current.added)
			if current.owned {
				heap.include(legacyNativeEventsBacking(cap(current.decoded)))
			}
			if current.previousOwned {
				heap.include(legacyNativeEventsBacking(cap(current.previous)))
			}
		}
		var next *legacyNativeEventList
		if current.object != nil {
			bytes, err := current.object.retainedBase()
			heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
			for _, child := range current.object.events {
				if child != nil {
					if next != nil {
						heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
						return
					}
					next = child
				}
			}
		}
		current = next
	}
}

func (native *legacyNativeObject) eventListField(field legacyNativeObjectField) bool {
	if !field.eventList {
		return false
	}
	if native.events != nil && native.events[native.field] != nil {
		return true
	}
	return native.destination.Elem().Field(field.index).Cap() == 0
}

func (native *legacyNativeObject) decodeEventList(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if native.events == nil {
		native.events = make([]*legacyNativeEventList, len(native.metadata.fields))
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	list := native.events[native.field]
	if !native.eventActive {
		if list == nil {
			var err error
			list, err = newLegacyNativeEventList(value.Interface().([]CIREvent), native.heaps[native.field], native.valueStart, native.valueEnd)
			if err != nil {
				return false, err
			}
			list.mutation = native.mutation
			native.events[native.field] = list
			list.paged, list.borrowed = native.pagedEvents, native.heaps[native.field]
			list.refreshHeap()
		} else {
			prior := value.Interface().([]CIREvent)
			if len(prior) != len(list.decoded) || cap(prior) != cap(list.decoded) || cap(prior) > 0 && unsafe.SliceData(prior) != unsafe.SliceData(list.decoded) ||
				native.heaps[native.field] != list.estimate {
				return false, ErrIntegrity
			}
			if err := list.start(native.valueStart, native.valueEnd); err != nil {
				return false, err
			}
			list.borrowed = native.heaps[native.field]
		}
		list.parentDepth, list.prefix = native.parentDepth+1, true
		list.frame = frame
		native.eventActive = true
	}
	if list.frame != frame {
		return false, ErrIntegrity
	}
	if list.phase != legacyNativeEventsDone {
		if input.eventsRunning != nil {
			input.eventsPending = list
			return false, nil
		}
		previousLimit := input.eventsFieldLimit
		input.eventsFieldLimit = native.fieldActionLimit()
		complete, err := list.step(ctx, input)
		input.eventsFieldLimit = previousLimit
		if err != nil || !complete {
			return false, err
		}
	}
	if !list.paged {
		value.Set(reflect.ValueOf(list.decoded))
	}
	native.position, native.valueEnd = list.position, list.position
	native.heaps[native.field], native.backingExcess[native.field] = list.estimate, list.excess
	native.eventActive, list.owned = false, false
	if list.paged {
		list.borrowed = list.estimate
	}
	list.added = docRangeHeaderHeapEstimate{}
	return true, nil
}
