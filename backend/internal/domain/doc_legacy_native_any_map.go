package domain

import (
	"unsafe"
	"weak"
)

type legacyNativeAnyMapOwnership struct {
	keyCapacity int
	valueBytes  int64
	valueExcess int64
	linkID      uint32
	owner       legacyNativeAnyValueOwner
}

type legacyNativeAnyMapLinks struct {
	canonicalKey string
	nextID       uint32
	dirtyNextID  uint32
	dirty        bool
}

type legacyNativeAnyMap struct {
	decoded      map[string]any
	ownership    map[string]legacyNativeAnyMapOwnership
	payload      docRangeHeaderHeapEstimate
	links        legacyCanonicalPages[legacyNativeAnyMapLinks]
	excess       int64
	firstID      uint32
	lastID       uint32
	dirtyHeadID  uint32
	dirtyTailID  uint32
	dirtyCount   int
	revision     uint64
	childBytes   int64
	resourceRead weak.Pointer[legacyNormalizationCapture]
	aliasCounter weak.Pointer[legacyNativeInitialAliasNode]
}

const legacyNativeAnyMapPutBytes = int(3*unsafe.Sizeof(legacyNativeAnyMapOwnership{}) + unsafe.Sizeof(legacyNativeAnyMapLinks{}) + unsafe.Sizeof(any(nil)) + 8*unsafe.Sizeof(uint32(0)) + unsafe.Sizeof(uint64(0)))

func (native *legacyNativeAnyMap) link(identity uint32) *legacyNativeAnyMapLinks {
	if native == nil || identity == 0 || uint64(identity) > uint64(native.links.length()) {
		return nil
	}
	return native.links.at(int(identity) - 1)
}

func (native *legacyNativeAnyMap) ownsLink(identity uint32) bool {
	node := native.link(identity)
	if node == nil {
		return false
	}
	row, found := native.ownership[node.canonicalKey]
	return found && row.linkID == identity
}

func (native *legacyNativeAnyMap) valid() bool {
	if native == nil || native.decoded == nil || native.ownership == nil || len(native.decoded) != len(native.ownership) ||
		!native.links.valid() || native.links.length() != len(native.ownership) || uint64(native.links.length()) > uint64(^uint32(0)) ||
		native.dirtyCount < 0 || native.dirtyCount > len(native.ownership) || native.revision < uint64(len(native.ownership)) || native.childBytes < 0 {
		return false
	}
	if len(native.ownership) == 0 {
		if native.firstID != 0 || native.lastID != 0 || native.revision != 0 {
			return false
		}
	} else {
		last := native.link(native.lastID)
		if native.firstID != 1 || native.lastID != uint32(native.links.length()) || last == nil || last.nextID != 0 {
			return false
		}
	}
	if native.dirtyCount == 0 {
		return native.dirtyHeadID == 0 && native.dirtyTailID == 0
	}
	head, tail := native.link(native.dirtyHeadID), native.link(native.dirtyTailID)
	return head != nil && tail != nil && head.dirty && tail.dirty && tail.dirtyNextID == 0
}

func legacyNativeAnyMapMetadata(count int, links docRangeHeaderHeapEstimate) docRangeHeaderHeapEstimate {
	estimate := legacyNativeAnyMapBacking(count, int64(unsafe.Sizeof(legacyNativeAnyMapOwnership{})))
	estimate.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyMap{})))
	estimate.include(links)
	return estimate
}

func newLegacyNativeAnyMap() *legacyNativeAnyMap {
	return &legacyNativeAnyMap{decoded: make(map[string]any), ownership: make(map[string]legacyNativeAnyMapOwnership)}
}

func legacyNativeAnyMapBacking(count int, valueWidth int64) docRangeHeaderHeapEstimate {
	var estimate docRangeHeaderHeapEstimate
	if count < 0 || valueWidth <= 0 {
		estimate.err = ErrIntegrity
		return estimate
	}
	width := docRangeHeaderHeapEstimate{bytes: int64(unsafe.Sizeof("")) + 8}
	width.include(docRangeHeaderHeapEstimate{bytes: valueWidth})
	if width.err != nil {
		return width
	}
	estimate.allocation(1, 64)
	estimate.allocation(int64(max(8, count)), width.bytes)
	return estimate
}

func (native *legacyNativeAnyMap) putBytes(key string) (int, error) {
	if !native.valid() {
		return 0, ErrIntegrity
	}
	if _, found := native.ownership[key]; found {
		return legacyNativeAnyMapPutBytes, nil
	}
	storage, err := native.links.appendBytes()
	if err != nil {
		return 0, err
	}
	return legacyNativeAnyMapPutBytes + storage, nil
}

func (native *legacyNativeAnyMap) put(key string, keyCapacity int, value any, valueHeap docRangeHeaderHeapEstimate, excess int64) error {
	_, err := native.putOwned(key, keyCapacity, value, valueHeap, excess)
	return err
}

func (native *legacyNativeAnyMap) putOwned(key string, keyCapacity int, value any, valueHeap docRangeHeaderHeapEstimate, excess int64) (docRangeHeaderHeapEstimate, error) {
	return native.putWithOwner(key, keyCapacity, value, valueHeap, excess, legacyNativeAnyValueOwner{}, nil)
}

func (native *legacyNativeAnyMap) putValue(key string, keyCapacity int, value legacyNativeAnyValue) (docRangeHeaderHeapEstimate, error) {
	return native.putValueGuarded(key, keyCapacity, value, nil)
}

func (native *legacyNativeAnyMap) putValueGuarded(key string, keyCapacity int, value legacyNativeAnyValue, guard func(docRangeHeaderHeapEstimate) error) (docRangeHeaderHeapEstimate, error) {
	if err := value.validateOwnership(); err != nil {
		return docRangeHeaderHeapEstimate{}, err
	}
	if value.owner.object == native {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	return native.putWithOwner(key, keyCapacity, value.value, value.heap, value.excess, value.owner, guard)
}

func (native *legacyNativeAnyMap) putWithOwner(key string, keyCapacity int, value any, valueHeap docRangeHeaderHeapEstimate, excess int64, owner legacyNativeAnyValueOwner, guard func(docRangeHeaderHeapEstimate) error) (docRangeHeaderHeapEstimate, error) {
	if native != nil {
		if err := legacyNativeResourceReadStatus(native.resourceRead); err != nil {
			return docRangeHeaderHeapEstimate{}, err
		}
	}
	if !native.valid() || native.revision == ^uint64(0) || native.payload.bytes < 0 || native.excess < 0 ||
		keyCapacity < len(key) || valueHeap.bytes < 0 || excess < 0 || excess > valueHeap.bytes {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	if native.payload.err != nil {
		return docRangeHeaderHeapEstimate{}, native.payload.err
	}
	if valueHeap.err != nil {
		return docRangeHeaderHeapEstimate{}, valueHeap.err
	}
	payload := native.payload
	extra := docRangeHeaderHeapEstimate{bytes: native.excess}
	count := len(native.decoded)
	var minimumKey docRangeHeaderHeapEstimate
	minimumKey.allocation(int64(len(key)), 1)
	if minimumKey.err != nil {
		return docRangeHeaderHeapEstimate{}, minimumKey.err
	}
	previous, found := native.ownership[key]
	if previous.owner.bytes < 0 || native.childBytes < previous.owner.bytes {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	children := docRangeHeaderHeapEstimate{bytes: native.childBytes - previous.owner.bytes}
	children.include(docRangeHeaderHeapEstimate{bytes: owner.bytes})
	if children.err != nil {
		return docRangeHeaderHeapEstimate{}, children.err
	}
	var previousLink *legacyNativeAnyMapLinks
	var linkHeap docRangeHeaderHeapEstimate
	if found {
		native.links.retained(&linkHeap)
		previousLink = native.link(previous.linkID)
		if previousLink == nil || previousLink.canonicalKey != key || previous.keyCapacity < len(key) ||
			previous.valueBytes < 0 || previous.valueExcess < 0 || previous.valueExcess > previous.valueBytes ||
			previousLink.dirty && native.dirtyCount == 0 {
			return docRangeHeaderHeapEstimate{}, ErrIntegrity
		}
		var previousKey docRangeHeaderHeapEstimate
		previousKey.allocation(int64(previous.keyCapacity), 1)
		previousPayload := previousKey
		previousPayload.include(docRangeHeaderHeapEstimate{bytes: previous.valueBytes})
		if previousPayload.err != nil || previousKey.bytes < minimumKey.bytes || payload.bytes < previousPayload.bytes {
			return docRangeHeaderHeapEstimate{}, ErrIntegrity
		}
		previousExtra := docRangeHeaderHeapEstimate{bytes: previousKey.bytes - minimumKey.bytes}
		previousExtra.include(docRangeHeaderHeapEstimate{bytes: previous.valueExcess})
		if previousExtra.err != nil || extra.bytes < previousExtra.bytes {
			return docRangeHeaderHeapEstimate{}, ErrIntegrity
		}
		payload.bytes -= previousPayload.bytes
		extra.bytes -= previousExtra.bytes
		keyCapacity = max(keyCapacity, previous.keyCapacity)
		key = previousLink.canonicalKey
	} else {
		if count == int(^uint(0)>>1) || uint64(count) >= uint64(^uint32(0)) {
			return docRangeHeaderHeapEstimate{}, ErrIntegrity
		}
		count++
		var err error
		_, linkHeap, err = native.links.appendRequirements()
		if err != nil {
			return docRangeHeaderHeapEstimate{}, err
		}
	}
	markDirty := !found || !previousLink.dirty
	if !found && native.lastID != 0 && !native.ownsLink(native.lastID) {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	if markDirty && native.dirtyCount != 0 {
		if !native.ownsLink(native.dirtyTailID) || native.dirtyHeadID != native.dirtyTailID && !native.ownsLink(native.dirtyHeadID) {
			return docRangeHeaderHeapEstimate{}, ErrIntegrity
		}
	}
	var keyHeap docRangeHeaderHeapEstimate
	keyHeap.allocation(int64(keyCapacity), 1)
	if keyHeap.err != nil || keyHeap.bytes < minimumKey.bytes {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	payload.include(keyHeap)
	payload.include(valueHeap)
	extra.include(docRangeHeaderHeapEstimate{bytes: keyHeap.bytes - minimumKey.bytes})
	extra.include(docRangeHeaderHeapEstimate{bytes: excess})
	if payload.err != nil {
		return docRangeHeaderHeapEstimate{}, payload.err
	}
	if extra.err != nil {
		return docRangeHeaderHeapEstimate{}, extra.err
	}
	retained := legacyNativeAnyMapBacking(count, int64(unsafe.Sizeof(any(nil))))
	retained.allocation(1, int64(unsafe.Sizeof(native.decoded)))
	retained.include(payload)
	retained.include(legacyNativeAnyMapMetadata(count, linkHeap))
	retained.include(children)
	if retained.err != nil {
		return docRangeHeaderHeapEstimate{}, retained.err
	}
	if guard != nil {
		if err := guard(retained); err != nil {
			return docRangeHeaderHeapEstimate{}, err
		}
	}
	if !found {
		if err := native.links.append(legacyNativeAnyMapLinks{canonicalKey: key}); err != nil {
			return docRangeHeaderHeapEstimate{}, err
		}
		previous.linkID = uint32(count)
		if native.lastID == 0 {
			native.firstID = previous.linkID
		} else {
			native.link(native.lastID).nextID = previous.linkID
		}
		native.lastID = previous.linkID
	}
	current := native.link(previous.linkID)
	if markDirty {
		if native.dirtyCount == 0 {
			native.dirtyHeadID = previous.linkID
		} else {
			native.link(native.dirtyTailID).dirtyNextID = previous.linkID
		}
		native.dirtyTailID = previous.linkID
		native.dirtyCount++
		current.dirty, current.dirtyNextID = true, 0
	}
	previous.keyCapacity, previous.valueBytes, previous.valueExcess = keyCapacity, valueHeap.bytes, excess
	previous.owner = owner
	native.decoded[key], native.ownership[key] = value, previous
	native.payload, native.excess = payload, extra.bytes
	native.childBytes = children.bytes
	native.revision++
	return retained, nil
}

func (native *legacyNativeAnyMap) values() map[string]any {
	if native == nil {
		return nil
	}
	return native.decoded
}

func (native *legacyNativeAnyMap) heap() docRangeHeaderHeapEstimate {
	if !native.valid() {
		return docRangeHeaderHeapEstimate{err: ErrIntegrity}
	}
	estimate := legacyNativeAnyMapBacking(len(native.decoded), int64(unsafe.Sizeof(any(nil))))
	estimate.allocation(1, int64(unsafe.Sizeof(native.decoded)))
	estimate.include(native.payload)
	return estimate
}

func (native *legacyNativeAnyMap) backingExcess() int64 {
	if native == nil {
		return 0
	}
	return native.excess
}

func (native *legacyNativeAnyMap) retained(estimate *docRangeHeaderHeapEstimate) {
	if native == nil {
		return
	}
	if !native.valid() {
		estimate.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	var linkHeap docRangeHeaderHeapEstimate
	native.links.retained(&linkHeap)
	estimate.include(legacyNativeAnyMapMetadata(len(native.ownership), linkHeap))
	estimate.include(docRangeHeaderHeapEstimate{bytes: native.childBytes})
}
