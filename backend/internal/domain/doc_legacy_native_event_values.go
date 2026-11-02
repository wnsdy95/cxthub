package domain

import "unsafe"

type legacyNativeEventValue struct {
	event       CIREvent
	replacement *legacyNativeEventValues
	blocks      *legacyNativeBlockValues
	heap        docRangeHeaderHeapEstimate
	excess      int64
}

type legacyNativeEventValues struct {
	records legacyCanonicalPages[legacyNativeEventValue]
	payload docRangeHeaderHeapEstimate
	count   int
}

func (values *legacyNativeEventValues) length() int {
	if values == nil {
		return 0
	}
	return values.count
}

func (values *legacyNativeEventValues) at(index int) *legacyNativeEventValue {
	if values == nil || index < 0 || index >= values.count {
		return nil
	}
	return values.records.at(index)
}

func (values *legacyNativeEventValues) backing() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if values == nil {
		return heap
	}
	if values.count < 0 || values.count > values.records.length() || !values.records.valid() {
		heap.err = ErrIntegrity
		return heap
	}
	heap.allocation(1, int64(unsafe.Sizeof(*values)))
	values.records.retained(&heap)
	return heap
}

func (values *legacyNativeEventValues) heap() docRangeHeaderHeapEstimate {
	heap := values.backing()
	if values != nil {
		heap.include(values.payload)
	}
	return heap
}

func (native *legacyNativeObject) pagedReplacement() (*legacyNativeEventValues, error) {
	if native == nil || !native.pagedEvents {
		return nil, nil
	}
	if native.metadata != &legacyEventNativeFields || native.phase != legacyNativeObjectDone || native.eventActive ||
		len(native.events) != 0 && len(native.events) != len(native.metadata.fields) {
		return nil, ErrIntegrity
	}
	for index, field := range native.metadata.fields {
		if !field.eventList {
			continue
		}
		if len(native.events) == 0 || native.events[index] == nil {
			if native.heaps[index].bytes != 0 {
				return nil, ErrIntegrity
			}
			return nil, nil
		}
		list := native.events[index]
		if !list.paged || list.phase != legacyNativeEventsDone || list.estimate != native.heaps[index] {
			return nil, ErrIntegrity
		}
		if list.values != nil && list.values.heap().err != nil {
			return nil, ErrIntegrity
		}
		return list.values, nil
	}
	return nil, nil
}
