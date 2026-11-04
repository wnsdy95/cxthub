package domain

import "unsafe"

type legacyNativeBlockValue struct {
	block  ContentBlock
	heaps  [2]docRangeHeaderHeapEstimate
	excess [2]int64
}

type legacyNativeBlockValues struct {
	records legacyCanonicalPages[legacyNativeBlockValue]
	payload docRangeHeaderHeapEstimate
	count   int
}

func (values *legacyNativeBlockValues) length() int {
	if values == nil {
		return 0
	}
	return values.count
}

func (values *legacyNativeBlockValues) at(index int) *legacyNativeBlockValue {
	if values == nil || index < 0 || index >= values.count {
		return nil
	}
	return values.records.at(index)
}

func (values *legacyNativeBlockValues) backing() docRangeHeaderHeapEstimate {
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

func (values *legacyNativeBlockValues) heap() docRangeHeaderHeapEstimate {
	heap := values.backing()
	if values != nil {
		heap.include(values.payload)
	}
	return heap
}

func (native *legacyNativeObject) pagedBlocks() (*legacyNativeBlockValues, error) {
	if native == nil || !native.pagedEvents {
		return nil, nil
	}
	if native.metadata != &legacyEventNativeFields || native.phase != legacyNativeObjectDone || native.blockActive ||
		len(native.blocks) != 0 && len(native.blocks) != len(native.metadata.fields) {
		return nil, ErrIntegrity
	}
	for index, field := range native.metadata.fields {
		if !field.blockList {
			continue
		}
		if len(native.blocks) == 0 || native.blocks[index] == nil {
			if native.heaps[index].bytes != 0 {
				return nil, ErrIntegrity
			}
			return nil, nil
		}
		list := native.blocks[index]
		if !list.paged || list.phase != legacyNativeBlocksDone || list.estimate != native.heaps[index] {
			return nil, ErrIntegrity
		}
		if list.data != nil && list.data.heap().err != nil {
			return nil, ErrIntegrity
		}
		return list.data, nil
	}
	return nil, nil
}
