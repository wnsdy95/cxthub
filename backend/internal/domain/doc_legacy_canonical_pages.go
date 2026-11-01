package domain

import (
	"math"
	"unsafe"
)

const legacyCanonicalInlineValues = 8
const legacyCanonicalPageValues = 64
const legacyCanonicalPageShift = 6
const legacyCanonicalPageMask = legacyCanonicalPageValues - 1

type legacyCanonicalPageNode[Value any] struct {
	children [legacyCanonicalPageValues]*legacyCanonicalPageNode[Value]
	values   *[legacyCanonicalPageValues]Value
}

type legacyCanonicalPages[Value any] struct {
	single *Value
	inline *[legacyCanonicalInlineValues]Value
	root   *legacyCanonicalPageNode[Value]
	count  int
	nodes  int
	pages  int
	depth  int
}

func legacyCanonicalStorageAllocation(heap *docRangeHeaderHeapEstimate, count int, width int64) {
	if count < 0 || width < 0 {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	if count == 0 || width == 0 {
		return
	}
	var unit docRangeHeaderHeapEstimate
	unit.allocation(1, width)
	if unit.err != nil || count < 0 || int64(count) > math.MaxInt64/unit.bytes {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	heap.include(docRangeHeaderHeapEstimate{bytes: int64(count) * unit.bytes})
}

func (values *legacyCanonicalPages[Value]) length() int {
	return values.count
}

func (values *legacyCanonicalPages[Value]) valid() bool {
	if values.count < 0 || values.nodes < 0 || values.pages < 0 || values.depth < 0 ||
		values.depth > (int(unsafe.Sizeof(uintptr(0)))*8-1)/legacyCanonicalPageShift {
		return false
	}
	if values.single != nil {
		return values.inline == nil && values.root == nil && values.nodes == 0 && values.pages == 0 && values.depth == 0 && values.count <= 1
	}
	if values.root == nil {
		return values.nodes == 0 && values.pages == 0 && values.depth == 0 &&
			(values.inline != nil && values.count <= legacyCanonicalInlineValues || values.inline == nil && values.count == 0)
	}
	return values.inline == nil && values.nodes > 0 && values.pages > 0 && values.pages <= values.nodes &&
		(values.count == 0 || (values.count-1)>>(legacyCanonicalPageShift*(values.depth+1)) == 0)
}

func (values *legacyCanonicalPages[Value]) appendBytes() (int, error) {
	storage, _, err := values.appendRequirements()
	return storage, err
}

func legacyCanonicalPagesHeap[Value any](single, inline bool, nodes, pages int) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if single {
		legacyCanonicalStorageAllocation(&heap, 1, int64(unsafe.Sizeof(*new(Value))))
	}
	if inline {
		legacyCanonicalStorageAllocation(&heap, 1, int64(unsafe.Sizeof([legacyCanonicalInlineValues]Value{})))
	}
	legacyCanonicalStorageAllocation(&heap, nodes, int64(unsafe.Sizeof(legacyCanonicalPageNode[Value]{})))
	legacyCanonicalStorageAllocation(&heap, pages, int64(unsafe.Sizeof([legacyCanonicalPageValues]Value{})))
	return heap
}

func (values *legacyCanonicalPages[Value]) appendRequirements() (int, docRangeHeaderHeapEstimate, error) {
	if !values.valid() || values.count == int(^uint(0)>>1) {
		return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	nodes, pages, copying := 0, 0, 0
	if values.root == nil {
		if values.inline == nil {
			inlineBytes := unsafe.Sizeof([legacyCanonicalInlineValues]Value{})
			valueBytes := unsafe.Sizeof(*new(Value))
			if inlineBytes > LegacyCanonicalEncodingBytes {
				return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
			}
			if values.single != nil {
				if values.count == 0 {
					return 0, legacyCanonicalPagesHeap[Value](true, false, 0, 0), nil
				}
				if inlineBytes > LegacyCanonicalEncodingBytes-valueBytes {
					return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
				}
				return int(inlineBytes + valueBytes), legacyCanonicalPagesHeap[Value](false, true, 0, 0), nil
			}
			if inlineBytes > LegacyCanonicalEncodingBytes-valueBytes {
				return int(inlineBytes), legacyCanonicalPagesHeap[Value](false, true, 0, 0), nil
			}
			return int(valueBytes), legacyCanonicalPagesHeap[Value](true, false, 0, 0), nil
		}
		if values.count < legacyCanonicalInlineValues {
			return 0, legacyCanonicalPagesHeap[Value](false, true, 0, 0), nil
		}
		nodes, pages, copying = 1, 1, legacyCanonicalInlineValues
	} else {
		depth, node := values.depth, values.root
		if values.count>>(legacyCanonicalPageShift*(depth+1)) != 0 {
			depth++
			node = nil
		}
		for level := depth; level >= 0; level-- {
			if node == nil {
				nodes++
			}
			if level == 0 {
				if node == nil || node.values == nil {
					pages++
				}
				break
			}
			if node != nil {
				node = node.children[(values.count>>(legacyCanonicalPageShift*level))&legacyCanonicalPageMask]
			}
		}
	}
	if nodes > int(^uint(0)>>1)-values.nodes || pages > int(^uint(0)>>1)-values.pages {
		return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	allocation := legacyCanonicalPagesHeap[Value](false, false, values.nodes+nodes, values.pages+pages)
	if allocation.err != nil {
		return 0, docRangeHeaderHeapEstimate{}, allocation.err
	}
	nodeBytes := unsafe.Sizeof(legacyCanonicalPageNode[Value]{})
	pageBytes := unsafe.Sizeof([legacyCanonicalPageValues]Value{})
	copyBytes := unsafe.Sizeof(*new(Value))
	if nodeBytes > LegacyCanonicalEncodingBytes || pageBytes > LegacyCanonicalEncodingBytes || copyBytes > LegacyCanonicalEncodingBytes {
		return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	bytes := int64(nodes)*int64(nodeBytes) + int64(pages)*int64(pageBytes) + int64(copying)*int64(copyBytes)
	if bytes < 0 || bytes > LegacyCanonicalEncodingBytes {
		return 0, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	return int(bytes), allocation, nil
}

func (values *legacyCanonicalPages[Value]) append(value Value) error {
	storage, err := values.appendBytes()
	if err != nil {
		return err
	}
	if values.root == nil {
		if values.inline == nil {
			if values.single != nil {
				if values.count == 0 {
					*values.single = value
					values.count = 1
					return nil
				}
				values.inline = new([legacyCanonicalInlineValues]Value)
				values.inline[0] = *values.single
				values.single = nil
			} else if storage == int(unsafe.Sizeof(value)) {
				values.single = new(Value)
				*values.single = value
				values.count = 1
				return nil
			} else {
				values.inline = new([legacyCanonicalInlineValues]Value)
			}
		}
		if values.count < legacyCanonicalInlineValues {
			values.inline[values.count] = value
			values.count++
			return nil
		}
		values.root = &legacyCanonicalPageNode[Value]{values: new([legacyCanonicalPageValues]Value)}
		copy(values.root.values[:legacyCanonicalInlineValues], values.inline[:])
		values.inline = nil
		values.nodes, values.pages = 1, 1
	}
	if values.count>>(legacyCanonicalPageShift*(values.depth+1)) != 0 {
		previous := values.root
		values.root = &legacyCanonicalPageNode[Value]{}
		values.root.children[0] = previous
		values.depth++
		values.nodes++
	}
	node := values.root
	for level := values.depth; level > 0; level-- {
		position := (values.count >> (legacyCanonicalPageShift * level)) & legacyCanonicalPageMask
		if node.children[position] == nil {
			node.children[position] = &legacyCanonicalPageNode[Value]{}
			values.nodes++
		}
		node = node.children[position]
	}
	if node.values == nil {
		node.values = new([legacyCanonicalPageValues]Value)
		values.pages++
	}
	node.values[values.count&legacyCanonicalPageMask] = value
	values.count++
	return nil
}

func (values *legacyCanonicalPages[Value]) at(position int) *Value {
	if position < 0 || position >= values.count {
		return nil
	}
	if values.single != nil {
		if values.inline != nil || values.root != nil || position != 0 {
			return nil
		}
		return values.single
	}
	if values.root == nil {
		if values.inline == nil || position >= legacyCanonicalInlineValues {
			return nil
		}
		return &values.inline[position]
	}
	node := values.root
	for level := values.depth; level > 0; level-- {
		node = node.children[(position>>(legacyCanonicalPageShift*level))&legacyCanonicalPageMask]
		if node == nil {
			return nil
		}
	}
	if node.values == nil {
		return nil
	}
	return &node.values[position&legacyCanonicalPageMask]
}

func (values *legacyCanonicalPages[Value]) pop() error {
	last := values.at(values.count - 1)
	if last == nil {
		return ErrIntegrity
	}
	var empty Value
	*last = empty
	values.count--
	return nil
}

func (values *legacyCanonicalPages[Value]) clear() {
	*values = legacyCanonicalPages[Value]{}
}

func (values *legacyCanonicalPages[Value]) retained(heap *docRangeHeaderHeapEstimate) {
	if !values.valid() {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	heap.include(legacyCanonicalPagesHeap[Value](values.single != nil, values.inline != nil, values.nodes, values.pages))
}

func (values *legacyCanonicalPages[Value]) ownedBytes() (int64, error) {
	var heap docRangeHeaderHeapEstimate
	values.retained(&heap)
	return heap.bytes, heap.err
}
