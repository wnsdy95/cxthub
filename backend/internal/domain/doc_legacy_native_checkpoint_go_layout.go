package domain

import (
	"context"
	"encoding/binary"
	"reflect"
	"unsafe"
)

type legacyNativeGoLayoutVisit struct {
	key    legacyNativeCheckpointGoGraphKey
	length int
	source []any
	group  *legacyNativeGoLayoutGroup
	active bool
}

type legacyNativeGoLayoutLookup struct {
	children [16]*legacyNativeGoLayoutLookup
	visit    *legacyNativeGoLayoutVisit
}

type legacyNativeGoLayoutGroup struct {
	start     uintptr
	end       uintptr
	views     int
	length    int
	source    []any
	entry     *legacyNativeCheckpointGoGraphEntry
	owner     *legacyNativeResourceOwner
	preparing bool
}

type legacyNativeGoLayoutFrame struct {
	parent   *legacyNativeGoLayoutFrame
	visit    *legacyNativeGoLayoutVisit
	source   any
	iterator *reflect.MapIter
	position int
}

type legacyNativeCheckpointGoLayout struct {
	parent      *legacyNativeCheckpointGoGraphSet
	work        *legacyNativeCheckpointGoGraph
	roots       []any
	copied      int
	root        *legacyNativeGoLayoutLookup
	frame       *legacyNativeGoLayoutFrame
	views       legacyCanonicalPages[legacyCanonicalKeyField]
	groups      legacyCanonicalPages[*legacyNativeGoLayoutGroup]
	backings    legacyCanonicalPages[*legacyNativeGoLayoutGroup]
	order       legacyCanonicalKeyOrder
	current     *legacyNativeGoLayoutGroup
	rootIndex   int
	position    int
	nodes       int64
	visits      int64
	depth       int
	allocations int64
	phase       uint8
	overlap     bool
	ready       bool
}

func (layout *legacyNativeCheckpointGoLayout) retainedBytes() (int64, error) {
	if layout == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*layout)))
	if layout.work != nil {
		heap.allocation(int64(cap(layout.roots)), int64(unsafe.Sizeof(any(nil))))
	}
	legacyCanonicalStorageAllocation(&heap, int(layout.nodes), int64(unsafe.Sizeof(legacyNativeGoLayoutLookup{})))
	legacyCanonicalStorageAllocation(&heap, int(layout.visits), int64(unsafe.Sizeof(legacyNativeGoLayoutVisit{})))
	legacyCanonicalStorageAllocation(&heap, layout.depth, int64(unsafe.Sizeof(legacyNativeGoLayoutFrame{})))
	legacyCanonicalStorageAllocation(&heap, layout.depth, int64(unsafe.Sizeof(reflect.MapIter{})))
	legacyCanonicalStorageAllocation(&heap, layout.depth, 256)
	layout.views.retained(&heap)
	legacyCanonicalStorageAllocation(&heap, layout.views.length(), int64(unsafe.Sizeof(uintptr(0))))
	layout.groups.retained(&heap)
	layout.backings.retained(&heap)
	legacyCanonicalStorageAllocation(&heap, layout.groups.length(), int64(unsafe.Sizeof(legacyNativeGoLayoutGroup{})))
	heap.include(docRangeHeaderHeapEstimate{bytes: layout.allocations})
	return heap.bytes, heap.err
}

func (layout *legacyNativeCheckpointGoLayout) admit(extra int64) error {
	if layout == nil {
		return ErrIntegrity
	}
	if layout.work != nil {
		return layout.work.admit(extra, 0)
	}
	if layout.parent == nil || layout.parent.layout != layout {
		return ErrIntegrity
	}
	return layout.parent.admit(extra)
}

func (layout *legacyNativeCheckpointGoLayout) lookup(key legacyNativeCheckpointGoGraphKey) (*legacyNativeGoLayoutVisit, int) {
	node := layout.root
	if node == nil {
		return nil, legacyNativeGoLayoutDigits + 1
	}
	for level := legacyNativeGoLayoutDigits - 1; level >= 0; level-- {
		node = node.children[legacyNativeGoLayoutDigit(key, level)]
		if node == nil {
			return nil, level + 1
		}
	}
	return node.visit, 0
}

func (layout *legacyNativeCheckpointGoLayout) push(ctx context.Context, value any) error {
	if layout.depth >= legacyNativeCheckpointGraphCaptureDepth {
		return ErrIntegrity
	}
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, value)
	if err != nil || key.kind == 0 {
		return err
	}
	visit, missing := layout.lookup(key)
	if missing == 0 {
		if visit == nil || visit.key != key || visit.active {
			return ErrIntegrity
		}
		return nil
	}
	if uint64(layout.visits) >= legacyNativeCheckpointNodes {
		return ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	legacyCanonicalStorageAllocation(&planned, missing, int64(unsafe.Sizeof(legacyNativeGoLayoutLookup{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeGoLayoutVisit{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeGoLayoutFrame{})))
	planned.allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
	planned.allocation(256, 1)
	if key.kind == legacyNativeCheckpointArray && key.capacity > 0 {
		_, future, failure := layout.views.appendRequirements()
		if failure != nil {
			return failure
		}
		var before docRangeHeaderHeapEstimate
		layout.views.retained(&before)
		if future.err != nil || before.err != nil || future.bytes < before.bytes {
			return ErrIntegrity
		}
		planned.include(docRangeHeaderHeapEstimate{bytes: future.bytes - before.bytes})
		planned.allocation(1, int64(unsafe.Sizeof(uintptr(0))))
	}
	if planned.err != nil {
		return planned.err
	}
	if err := layout.admit(planned.bytes); err != nil {
		return err
	}
	visit = &legacyNativeGoLayoutVisit{key: key, length: length, active: true}
	if layout.root == nil {
		layout.root = &legacyNativeGoLayoutLookup{}
		layout.nodes++
	}
	node := layout.root
	for level := legacyNativeGoLayoutDigits - 1; level >= 0; level-- {
		position := legacyNativeGoLayoutDigit(key, level)
		if node.children[position] == nil {
			node.children[position] = &legacyNativeGoLayoutLookup{}
			layout.nodes++
		}
		node = node.children[position]
	}
	node.visit = visit
	layout.visits++
	frame := &legacyNativeGoLayoutFrame{parent: layout.frame, visit: visit, source: value}
	if key.kind == legacyNativeCheckpointObject {
		frame.iterator = reflect.ValueOf(value).MapRange()
	} else if key.capacity > 0 {
		visit.source = value.([]any)
		var address [unsafe.Sizeof(uintptr(0))]byte
		if len(address) == 8 {
			binary.BigEndian.PutUint64(address[:], uint64(key.address))
		} else {
			binary.BigEndian.PutUint32(address[:], uint32(key.address))
		}
		if err := layout.views.append(legacyCanonicalKeyField{key: string(address[:]), value: visit}); err != nil {
			return err
		}
	}
	layout.frame, layout.depth = frame, layout.depth+1
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) discover(ctx context.Context) error {
	frame := layout.frame
	if frame == nil {
		if layout.rootIndex == len(layout.roots) {
			layout.root, layout.roots = nil, nil
			layout.nodes, layout.visits = 0, int64(layout.views.length())
			layout.order = newLegacyCanonicalKeyOrder(layout.views.length(), false)
			layout.phase = 1
			return nil
		}
		value := layout.roots[layout.rootIndex]
		layout.rootIndex++
		return layout.push(ctx, value)
	}
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, frame.source)
	if err != nil {
		return err
	}
	if key != frame.visit.key || length != frame.visit.length {
		return ErrConflict
	}
	if frame.iterator != nil {
		if frame.iterator.Next() {
			return layout.push(ctx, frame.iterator.Value().Interface())
		}
	} else {
		if frame.position < key.capacity {
			value := frame.source.([]any)[:key.capacity][frame.position]
			frame.position++
			before := layout.frame
			if err := layout.push(ctx, value); err != nil {
				return err
			}
			if layout.frame != before {
				return nil
			}
		}
		if frame.position < key.capacity {
			return nil
		}
	}
	frame.visit.active = false
	layout.frame, layout.depth = frame.parent, layout.depth-1
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) groupNext() error {
	if layout.position == layout.views.length() {
		if !layout.overlap {
			*layout = legacyNativeCheckpointGoLayout{parent: layout.parent, work: layout.work, ready: true}
			return nil
		}
		layout.position, layout.phase = 0, 3
		return nil
	}
	field := layout.views.at(layout.position)
	if field == nil {
		return ErrIntegrity
	}
	visit, ok := field.value.(*legacyNativeGoLayoutVisit)
	if !ok || visit == nil || visit.key.capacity <= 0 || visit.active {
		return ErrIntegrity
	}
	const width = uintptr(unsafe.Sizeof(any(nil)))
	start, end := visit.key.address, visit.key.address+uintptr(visit.key.capacity)*width
	group := layout.current
	if group == nil || start >= group.end {
		_, future, err := layout.groups.appendRequirements()
		if err != nil {
			return err
		}
		var current docRangeHeaderHeapEstimate
		layout.groups.retained(&current)
		if future.err != nil || current.err != nil || future.bytes < current.bytes {
			return ErrIntegrity
		}
		planned := docRangeHeaderHeapEstimate{bytes: future.bytes - current.bytes}
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeGoLayoutGroup{})))
		if planned.err != nil {
			return planned.err
		}
		if err := layout.admit(planned.bytes); err != nil {
			return err
		}
		group = &legacyNativeGoLayoutGroup{start: start, end: end}
		if err := layout.groups.append(group); err != nil {
			return err
		}
		layout.current = group
	} else {
		if (start-group.start)%width != 0 || uint64((max(group.end, end)-group.start)/width) > legacyNativeCheckpointNodes {
			return ErrIntegrity
		}
		group.end = max(group.end, end)
		layout.overlap = true
	}
	group.views++
	visit.group = group
	layout.position++
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) allocateNext() error {
	if layout.position == layout.groups.length() {
		layout.position, layout.phase = 0, 4
		return nil
	}
	group := *layout.groups.at(layout.position)
	if group == nil || group.views <= 0 || group.end <= group.start {
		return ErrIntegrity
	}
	if group.views > 1 {
		capacity := int((group.end - group.start) / uintptr(unsafe.Sizeof(any(nil))))
		heap := legacyNativeAnyArrayBacking(capacity)
		if heap.err != nil {
			return heap.err
		}
		if err := layout.admit(heap.bytes); err != nil {
			return err
		}
		group.source = make([]any, capacity)
		layout.allocations += heap.bytes
	}
	layout.position++
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) step(ctx context.Context) error {
	for count := 0; count < 4 && !layout.ready; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		switch layout.phase {
		case 0:
			for transitions := 0; transitions < legacyNativeCheckpointPageItems/4 && layout.phase == 0; transitions++ {
				err = layout.discover(ctx)
				if err != nil {
					break
				}
			}
		case 1:
			remaining := canonicalCheckBytes / 4
			for transitions := 0; transitions < legacyNativeCheckpointPageItems/4; transitions++ {
				used, ready, yield, failure := layout.order.advance(&layout.views, remaining)
				if failure != nil {
					err = failure
					break
				}
				remaining -= used
				if ready {
					layout.phase = 2
					break
				}
				if yield {
					break
				}
			}
		case 2:
			err = layout.groupNext()
		case 3:
			err = layout.allocateNext()
		case 4:
			err = layout.copyNext(ctx)
		case 5:
			err = layout.retainBacking()
		default:
			return ErrIntegrity
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) retainBacking() error {
	if layout.position == layout.groups.length() {
		*layout = legacyNativeCheckpointGoLayout{parent: layout.parent, work: layout.work,
			groups: layout.backings, allocations: layout.allocations, ready: true}
		return nil
	}
	group := *layout.groups.at(layout.position)
	if group.source != nil {
		_, future, err := layout.backings.appendRequirements()
		if err != nil {
			return err
		}
		var current docRangeHeaderHeapEstimate
		layout.backings.retained(&current)
		if future.err != nil || current.err != nil || future.bytes < current.bytes {
			return ErrIntegrity
		}
		if err := layout.admit(future.bytes - current.bytes); err != nil {
			return err
		}
		group.source = group.source[:group.length]
		if err := layout.backings.append(group); err != nil {
			return err
		}
	}
	layout.position++
	return nil
}

func (layout *legacyNativeCheckpointGoLayout) backing(key legacyNativeCheckpointGoGraphKey) (*legacyNativeGoLayoutGroup, error) {
	if !layout.ready || key.kind != legacyNativeCheckpointArray {
		return nil, ErrIntegrity
	}
	if key.capacity == 0 {
		return nil, nil
	}
	const width = uintptr(unsafe.Sizeof(any(nil)))
	end := key.address + uintptr(key.capacity)*width
	lower, upper := 0, layout.groups.length()
	for lower < upper {
		middle := lower + (upper-lower)/2
		group := *layout.groups.at(middle)
		if key.address >= group.end {
			lower = middle + 1
		} else if end <= group.start {
			upper = middle
		} else {
			if key.address < group.start || end > group.end || (key.address-group.start)%width != 0 {
				return nil, ErrConflict
			}
			return group, nil
		}
	}
	return nil, nil
}

func (layout *legacyNativeCheckpointGoLayout) close() {
	if layout != nil {
		*layout = legacyNativeCheckpointGoLayout{}
	}
}

const legacyNativeGoLayoutDigits = (legacyNativeCheckpointGoGraphKeyBits + 3) / 4

func legacyNativeGoLayoutDigit(key legacyNativeCheckpointGoGraphKey, level int) uintptr {
	var digit uintptr
	for bit := 0; bit < 4; bit++ {
		digit |= key.bit(level*4+bit) << uint(bit)
	}
	return digit
}

func (layout *legacyNativeCheckpointGoLayout) copyNext(ctx context.Context) error {
	if layout.position == layout.views.length() {
		layout.position, layout.phase = 0, 5
		return nil
	}
	visit := layout.views.at(layout.position).value.(*legacyNativeGoLayoutVisit)
	group := visit.group
	if group.source == nil {
		layout.position++
		return nil
	}
	key, length, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, visit.source)
	if err != nil {
		return err
	}
	if key != visit.key || length != visit.length {
		return ErrConflict
	}
	offset := int((key.address - group.start) / uintptr(unsafe.Sizeof(any(nil))))
	end := min(key.capacity, layout.copied+legacyNativeCheckpointPageItems/4)
	for position := layout.copied; position < end; position++ {
		value := visit.source[:key.capacity][position]
		group.source[offset+position] = value
		if value != nil {
			group.length = max(group.length, offset+position+1)
		}
	}
	layout.copied = end
	if end == key.capacity {
		layout.position++
		layout.copied = 0
	}
	return nil
}
