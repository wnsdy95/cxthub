package domain

import (
	"context"
	"unsafe"
)

type legacyNativeAnyArrayTail struct {
	start      int
	children   legacyCanonicalPages[legacyNativeAnyArrayChild]
	estimate   docRangeHeaderHeapEstimate
	excess     int64
	childBytes int64
}

func (tail *legacyNativeAnyArrayTail) retained(estimate *docRangeHeaderHeapEstimate) {
	if tail == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*tail)))
	tail.children.retained(estimate)
	estimate.include(docRangeHeaderHeapEstimate{bytes: tail.childBytes})
}

func (list *legacyNativeAnyArray) validateTail(complete bool) error {
	if list == nil || list.previous != nil || cap(list.previous) != 0 || list.maximum < 0 || cap(list.decoded) > list.maximum ||
		list.childBytes < 0 || list.revision < uint64(len(list.decoded)) {
		return ErrIntegrity
	}
	if list.tail == nil {
		if complete && len(list.decoded) != cap(list.decoded) {
			return ErrIntegrity
		}
		return nil
	}
	tail := list.tail
	if !list.tracked || !list.children.valid() || list.children.length() != len(list.decoded) ||
		tail.start != len(list.decoded) || tail.start >= cap(list.decoded) || !tail.children.valid() ||
		tail.children.length() <= 0 || tail.children.length() > cap(list.decoded)-tail.start || tail.childBytes < 0 ||
		list.children.length() == 0 && list.childBytes != 0 ||
		list.revision < uint64(tail.start+tail.children.length()) ||
		complete && tail.children.length() != cap(list.decoded)-tail.start {
		return ErrIntegrity
	}
	if tail.estimate.err != nil {
		return tail.estimate.err
	}
	if tail.estimate.bytes < 0 || tail.excess < 0 || tail.excess > tail.estimate.bytes ||
		list.excess < 0 || list.excess > list.estimate.bytes || tail.children.at(tail.children.length()-1) == nil {
		return ErrIntegrity
	}
	minimum := legacyNativeAnyArrayBacking(cap(list.decoded))
	minimum.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	if minimum.err != nil || list.estimate.bytes < minimum.bytes {
		return ErrIntegrity
	}
	retained := list.heap()
	list.retained(&retained)
	if retained.err != nil {
		return retained.err
	}
	if list.backingExcess() < 0 {
		return ErrIntegrity
	}
	return nil
}

func (list *legacyNativeAnyArray) appendTailValueGuarded(ctx context.Context, input *legacyNormalizationInput, value legacyNativeAnyValue, guard func(docRangeHeaderHeapEstimate) error) (bool, error) {
	if ctx == nil {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list != nil {
		if err := legacyNativeResourceReadStatus(list.resourceRead); err != nil {
			return false, err
		}
	}
	if list == nil || input == nil || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes ||
		list.revision == ^uint64(0) {
		return false, ErrIntegrity
	}
	if err := list.validateTail(false); err != nil {
		return false, err
	}
	if !list.children.valid() || list.children.length() != len(list.decoded) || len(list.decoded) != 0 && !list.tracked ||
		len(list.decoded) == 0 && list.childBytes != 0 || list.excess < 0 || list.excess > list.estimate.bytes {
		return false, ErrIntegrity
	}
	minimum := legacyNativeAnyArrayBacking(cap(list.decoded))
	minimum.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	if minimum.err != nil || list.estimate.bytes < minimum.bytes {
		return false, ErrIntegrity
	}
	if err := value.validateOwnership(); err != nil {
		return false, err
	}
	if value.owner.array == list {
		return false, ErrIntegrity
	}
	var children legacyCanonicalPages[legacyNativeAnyArrayChild]
	var payload docRangeHeaderHeapEstimate
	var excess docRangeHeaderHeapEstimate
	var owners docRangeHeaderHeapEstimate
	position := len(list.decoded)
	tail := list.tail
	revision := list.revision
	if tail != nil {
		children, payload = tail.children, tail.estimate
		excess.bytes, owners.bytes = tail.excess, tail.childBytes
		position += children.length()
	}
	if position >= cap(list.decoded) {
		return false, ErrIntegrity
	}
	if previous := list.decoded[:cap(list.decoded)][position]; list.resource == nil && previous != nil || list.resource != nil && !legacyNativeCheckpointGoGraphSame(previous, value.value) {
		return false, ErrIntegrity
	}
	storageBytes, metadata, err := children.appendRequirements()
	if err != nil {
		return false, err
	}
	payload.include(value.heap)
	excess.include(docRangeHeaderHeapEstimate{bytes: value.excess})
	owners.include(docRangeHeaderHeapEstimate{bytes: value.owner.bytes})
	if payload.err != nil {
		return false, payload.err
	}
	if excess.err != nil {
		return false, excess.err
	}
	if owners.err != nil {
		return false, owners.err
	}
	nextHeap := list.estimate
	nextHeap.include(payload)
	nextExcess := docRangeHeaderHeapEstimate{bytes: list.excess}
	nextExcess.include(excess)
	if nextHeap.err != nil {
		return false, nextHeap.err
	}
	if nextExcess.err != nil || nextExcess.bytes > nextHeap.bytes {
		return false, ErrIntegrity
	}
	retained := nextHeap
	retained.allocation(1, int64(unsafe.Sizeof(*list)))
	list.children.retained(&retained)
	retained.include(docRangeHeaderHeapEstimate{bytes: list.childBytes})
	retained.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyArrayTail{})))
	retained.include(metadata)
	retained.include(owners)
	if list.resource != nil {
		bytes, err := list.resource.retainedBytes()
		retained.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if children.single != nil && children.length() == 1 || children.inline != nil && children.length() == legacyCanonicalInlineValues {
		children.retained(&retained)
	}
	if retained.err != nil {
		return false, retained.err
	}
	if guard != nil {
		if err := guard(retained); err != nil {
			return false, err
		}
	}
	const width = int(unsafe.Sizeof(any(nil)))
	child := legacyNativeAnyArrayChild{heapBytes: value.heap.bytes, excess: value.excess, capacity: value.capacity, owner: value.owner}
	workBytes := storageBytes + int(unsafe.Sizeof(child)) + width
	if tail == nil {
		workBytes += int(unsafe.Sizeof(legacyNativeAnyArrayTail{}))
	}
	if workBytes < 0 || workBytes > ConversationManifestChunkBytes {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list.revision != revision || list.tail != tail {
		return false, ErrConflict
	}
	if err := value.validateOwnership(); err != nil {
		return false, err
	}
	if ConversationManifestChunkBytes-input.workBytes < workBytes {
		return false, nil
	}
	if err := children.append(child); err != nil {
		return false, err
	}
	if tail == nil {
		tail = &legacyNativeAnyArrayTail{start: len(list.decoded)}
	}
	list.decoded[:cap(list.decoded)][position] = value.value
	tail.children, tail.estimate = children, payload
	tail.excess, tail.childBytes = excess.bytes, owners.bytes
	list.tail, list.tracked = tail, true
	input.workBytes += workBytes
	list.revision++
	return true, nil
}

func (list *legacyNativeAnyArray) tailValue(ctx context.Context, physicalPosition int) (legacyNativeAnyValue, error) {
	if ctx == nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if err := list.validateTail(false); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if list.tail == nil || physicalPosition < list.tail.start || physicalPosition-list.tail.start >= list.tail.children.length() {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	child := list.tail.children.at(physicalPosition - list.tail.start)
	if child == nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	value := legacyNativeAnyValue{value: list.decoded[:cap(list.decoded)][physicalPosition], heap: docRangeHeaderHeapEstimate{bytes: child.heapBytes},
		excess: child.excess, capacity: child.capacity, owner: child.owner}
	if err := value.validateOwnership(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	return value, nil
}
