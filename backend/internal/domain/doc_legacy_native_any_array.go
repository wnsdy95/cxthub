package domain

import (
	"context"
	"unsafe"
	"weak"
)

type legacyNativeAnyArray struct {
	decoded      []any
	previous     []any
	estimate     docRangeHeaderHeapEstimate
	excess       int64
	maximum      int
	copied       int
	children     legacyCanonicalPages[legacyNativeAnyArrayChild]
	childBytes   int64
	tail         *legacyNativeAnyArrayTail
	revision     uint64
	tracked      bool
	resourceRead weak.Pointer[legacyNormalizationCapture]
	aliasCounter weak.Pointer[legacyNativeInitialAliasNode]
	resource     *legacyNativeResourceOwner
	offset       int
}

func newLegacyNativeAnyArray(maximum int) (*legacyNativeAnyArray, error) {
	const width = int(unsafe.Sizeof(any(nil)))
	if maximum < 0 || maximum > int(^uint(0)>>1)/width {
		return nil, ErrIntegrity
	}
	list := &legacyNativeAnyArray{decoded: make([]any, 0), maximum: maximum}
	list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	return list, nil
}

func legacyNativeAnyArrayBacking(capacity int) docRangeHeaderHeapEstimate {
	const width = int(unsafe.Sizeof(any(nil)))
	if capacity < 0 || capacity > int(^uint(0)>>1)/width {
		return docRangeHeaderHeapEstimate{err: ErrIntegrity}
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(int64(capacity), int64(width))
	return estimate
}

func (list *legacyNativeAnyArray) append(ctx context.Context, input *legacyNormalizationInput, value any, valueHeap docRangeHeaderHeapEstimate, excess int64) (bool, error) {
	return list.appendWithChild(ctx, input, value, valueHeap, excess, nil, nil)
}

func (list *legacyNativeAnyArray) appendValue(ctx context.Context, input *legacyNormalizationInput, value legacyNativeAnyValue) (bool, error) {
	return list.appendValueGuarded(ctx, input, value, nil)
}

func (list *legacyNativeAnyArray) appendValueGuarded(ctx context.Context, input *legacyNormalizationInput, value legacyNativeAnyValue, guard func(docRangeHeaderHeapEstimate) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := value.validateOwnership(); err != nil {
		return false, err
	}
	if value.owner.array == list {
		return false, ErrIntegrity
	}
	child := legacyNativeAnyArrayChild{heapBytes: value.heap.bytes, excess: value.excess, capacity: value.capacity, owner: value.owner}
	return list.appendWithChild(ctx, input, value.value, value.heap, value.excess, &child, guard)
}

func (list *legacyNativeAnyArray) appendWithChild(ctx context.Context, input *legacyNormalizationInput, value any, valueHeap docRangeHeaderHeapEstimate, excess int64, child *legacyNativeAnyArrayChild, guard func(docRangeHeaderHeapEstimate) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list != nil {
		if err := legacyNativeResourceReadStatus(list.resourceRead); err != nil {
			return false, err
		}
	}
	if list == nil || input == nil || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || len(list.decoded) >= list.maximum ||
		list.revision == ^uint64(0) || list.childBytes < 0 || list.tracked && child == nil || list.tail != nil {
		return false, ErrIntegrity
	}
	if valueHeap.err != nil {
		return false, valueHeap.err
	}
	if valueHeap.bytes < 0 || excess < 0 {
		return false, ErrIntegrity
	}
	if list.resource != nil {
		expected, err := list.resourceValue(ctx, len(list.decoded))
		if err != nil {
			return false, err
		}
		if !legacyNativeCheckpointGoGraphSame(expected.value, value) {
			return false, ErrConflict
		}
	}
	var childCost int
	children := docRangeHeaderHeapEstimate{bytes: list.childBytes}
	var metadata docRangeHeaderHeapEstimate
	list.children.retained(&metadata)
	if child != nil {
		if !list.children.valid() || list.children.length() != len(list.decoded) {
			return false, ErrIntegrity
		}
		cost, future, err := list.children.appendRequirements()
		if err != nil {
			return false, err
		}
		childCost = cost + int(unsafe.Sizeof(*child))
		children.include(docRangeHeaderHeapEstimate{bytes: child.owner.bytes})
		metadata = future
	}
	nextHeap := list.estimate
	nextHeap.include(valueHeap)
	if nextHeap.err != nil {
		return false, nextHeap.err
	}
	nextExcess := docRangeHeaderHeapEstimate{bytes: list.excess}
	nextExcess.include(docRangeHeaderHeapEstimate{bytes: excess})
	if nextExcess.err != nil {
		return false, nextExcess.err
	}
	if err := list.validateRetained(nextHeap, metadata, children, cap(list.previous), guard); err != nil {
		return false, err
	}
	const width = int(unsafe.Sizeof(any(nil)))
	if list.previous == nil && len(list.decoded) == cap(list.decoded) {
		if list.revision == ^uint64(0)-1 {
			return false, ErrIntegrity
		}
		if ConversationManifestChunkBytes-input.workBytes < 2*width {
			return false, nil
		}
		capacity := list.maximum
		if cap(list.decoded) <= list.maximum/2 {
			capacity = max(len(list.decoded)+1, 2*cap(list.decoded))
		}
		oldBacking := legacyNativeAnyArrayBacking(cap(list.decoded))
		if oldBacking.err != nil || list.estimate.bytes < oldBacking.bytes {
			return false, ErrIntegrity
		}
		nextHeap.bytes -= oldBacking.bytes
		nextHeap.include(legacyNativeAnyArrayBacking(capacity))
		if nextHeap.err != nil {
			return false, nextHeap.err
		}
		if err := list.validateRetained(nextHeap, metadata, children, cap(list.decoded), guard); err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		next := make([]any, len(list.decoded), capacity)
		list.previous, list.decoded, list.copied = list.decoded, next, 0
		list.revision++
		list.estimate = docRangeHeaderHeapEstimate{bytes: nextHeap.bytes - valueHeap.bytes}
		if len(list.previous) == 0 {
			list.previous = nil
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
		input.workBytes += 2 * width * length
		if list.copied == len(list.previous) {
			list.previous = nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if ConversationManifestChunkBytes-input.workBytes < width+childCost {
		return false, nil
	}
	if child != nil {
		if err := list.children.append(*child); err != nil {
			return false, err
		}
		list.tracked, list.childBytes = true, children.bytes
		input.workBytes += childCost
	}
	length := len(list.decoded)
	list.decoded = list.decoded[:length+1]
	list.decoded[length] = value
	list.estimate, list.excess = nextHeap, nextExcess.bytes
	input.workBytes += width
	list.revision++
	return true, nil
}

func (list *legacyNativeAnyArray) values() []any {
	if list.previous != nil {
		return list.previous
	}
	return list.decoded
}

func (list *legacyNativeAnyArray) validateRetained(payload, metadata, children docRangeHeaderHeapEstimate, previousCapacity int, guard func(docRangeHeaderHeapEstimate) error) error {
	retained := payload
	retained.allocation(1, int64(unsafe.Sizeof(*list)))
	retained.include(metadata)
	retained.include(children)
	retained.include(legacyNativeAnyArrayBacking(previousCapacity))
	if list.resource != nil {
		bytes, err := list.resource.retainedBytes()
		retained.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if retained.err != nil {
		return retained.err
	}
	if guard != nil {
		return guard(retained)
	}
	return nil
}

func (list *legacyNativeAnyArray) heap() docRangeHeaderHeapEstimate {
	estimate := list.estimate
	if list.tail != nil {
		estimate.include(list.tail.estimate)
	}
	return estimate
}

func (list *legacyNativeAnyArray) backingExcess() int64 {
	if list.tail == nil {
		return list.excess
	}
	excess := docRangeHeaderHeapEstimate{bytes: list.excess}
	excess.include(docRangeHeaderHeapEstimate{bytes: list.tail.excess})
	if excess.err != nil {
		return -1
	}
	return excess.bytes
}

func (list *legacyNativeAnyArray) retained(estimate *docRangeHeaderHeapEstimate) {
	if list == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*list)))
	estimate.include(legacyNativeAnyArrayBacking(cap(list.previous)))
	list.children.retained(estimate)
	estimate.include(docRangeHeaderHeapEstimate{bytes: list.childBytes})
	list.tail.retained(estimate)
	if list.resource != nil {
		retained, err := list.resource.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
}
