package domain

import (
	"context"
	"math"
	"reflect"
	"unsafe"
	"weak"
)

type legacyNativeInitialAlias struct {
	input  uintptr
	output legacyNativeCheckpointGoGraphKey
	count  int64
}

type legacyNativeInitialAliasNode struct {
	work     *legacyNativeInitialAliasPreparation
	owner    legacyNativeAnyValueOwner
	parent   *legacyNativeInitialAliasNode
	next     *legacyNativeInitialAliasNode
	cursor   *legacyNativeAnyMapCheckpoint
	position int
	count    int64
	complete bool
}

type legacyNativeInitialAliasPreparation struct {
	parent  *legacyNativeObjectCheckpointRestore
	input   *legacyNativeAnyMap
	first   *legacyNativeInitialAliasNode
	current *legacyNativeInitialAliasNode
	binding legacyNativeInitialAlias
	nodes   int64
	cursors int64
	ready   bool
	closed  bool
}

func (work *legacyNativeObjectCheckpointRestore) prepareInitialAlias(ctx context.Context) (bool, error) {
	if work.state.Model != legacyNativeCheckpointEventModel || work.state.InitialGraphCount == 0 {
		return true, nil
	}
	if work.aliasTask == nil {
		input := work.native.metadata.match("input")
		output := work.native.metadata.match("output")
		if input < 0 || output < 0 || !work.native.metadata.fields[input].genericMap || !work.native.metadata.fields[output].interfaceString {
			return false, ErrIntegrity
		}
		inputSlot, outputSlot := work.state.Fields[input].InitialGraph, work.state.Fields[output].InitialGraph
		if inputSlot == 0 || outputSlot == 0 {
			return true, nil
		}
		if int(inputSlot) > len(work.initialValues) || int(outputSlot) > len(work.initialValues) {
			return false, ErrIntegrity
		}
		source, target := work.initialValues[inputSlot-1], work.initialValues[outputSlot-1]
		if err := source.validateOwnership(); err != nil {
			return false, err
		}
		if err := target.validateOwnership(); err != nil {
			return false, err
		}
		if source.owner.object == nil {
			return false, ErrIntegrity
		}
		key, _, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, target.value)
		if err != nil {
			return false, err
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeInitialAliasPreparation{})))
		if planned.err != nil {
			return false, planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return false, err
		}
		current := &legacyNativeInitialAliasPreparation{parent: work, input: source.owner.object,
			binding: legacyNativeInitialAlias{input: reflect.ValueOf(source.value).Pointer(), output: key}}
		work.aliasTask = current
		if err := current.attach(target.owner); err != nil {
			return false, err
		}
		return false, nil
	}
	current := work.aliasTask
	for count := 0; count < 4 && !current.ready; count++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := current.advance(ctx); err != nil {
			return false, err
		}
	}
	if !current.ready {
		return false, nil
	}
	if err := work.admit(0); err != nil {
		return false, err
	}
	work.native.initialAlias = current.binding
	current.close()
	work.aliasTask = nil
	return true, nil
}

func (work *legacyNativeInitialAliasPreparation) add(count int64) error {
	if count < 0 {
		return ErrIntegrity
	}
	if work.current == nil {
		work.binding.count, work.ready = count, true
		return nil
	}
	if work.current.count > math.MaxInt64-count {
		return ErrIntegrity
	}
	work.current.count += count
	return nil
}

func (work *legacyNativeInitialAliasPreparation) attach(owner legacyNativeAnyValueOwner) error {
	if err := owner.validate(); err != nil {
		return err
	}
	if owner.object == work.input && owner.array == nil {
		return work.add(1)
	}
	if owner.object == nil && owner.array == nil {
		return nil
	}
	var cached *legacyNativeInitialAliasNode
	if owner.object != nil {
		cached = owner.object.aliasCounter.Value()
	} else {
		cached = owner.array.aliasCounter.Value()
	}
	if cached != nil && cached.work == work {
		if cached.owner != owner || !cached.complete {
			return ErrIntegrity
		}
		return work.add(cached.count)
	}
	if uint64(work.nodes) >= legacyNativeCheckpointNodes {
		return ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeInitialAliasNode{})))
	if planned.err != nil {
		return planned.err
	}
	if err := work.parent.admit(planned.bytes); err != nil {
		return err
	}
	node := &legacyNativeInitialAliasNode{work: work, owner: owner, parent: work.current, next: work.first}
	if owner.object != nil {
		owner.object.aliasCounter = weak.Make(node)
	} else {
		owner.array.aliasCounter = weak.Make(node)
	}
	work.first, work.current = node, node
	work.nodes++
	return nil
}

func (work *legacyNativeInitialAliasPreparation) advance(ctx context.Context) error {
	if work.closed || work.parent == nil || work.parent.aliasTask != work || work.parent.native == nil || work.parent.phase != 7 {
		return ErrConflict
	}
	node := work.current
	if node == nil {
		return ErrIntegrity
	}
	if err := node.owner.validate(); err != nil {
		return err
	}
	if node.owner.object != nil {
		if node.cursor == nil {
			var planned docRangeHeaderHeapEstimate
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyMapCheckpoint{})))
			if planned.err != nil {
				return planned.err
			}
			if err := work.parent.admit(planned.bytes); err != nil {
				return err
			}
			cursor, err := node.owner.mapCheckpoint(ctx, true)
			if err != nil {
				return err
			}
			node.cursor = cursor
			work.cursors++
			return nil
		}
		entry, found, err := node.cursor.next(ctx)
		if err != nil {
			return err
		}
		if found {
			return work.attach(entry.owner)
		}
		node.cursor.close()
		node.cursor = nil
		work.cursors--
	} else if node.position < cap(node.owner.array.decoded) {
		array := node.owner.array
		if node.position >= len(array.decoded) && array.tail == nil {
			node.position = cap(array.decoded)
			return nil
		}
		var child legacyNativeAnyValue
		var err error
		if node.position < len(array.decoded) {
			child, err = node.owner.arrayValue(ctx, node.position)
		} else if array.tail != nil {
			child, err = array.tailValue(ctx, node.position)
		}
		if err != nil {
			return err
		}
		node.position++
		return work.attach(child.owner)
	}
	node.complete = true
	work.current = node.parent
	return work.add(node.count)
}

func (work *legacyNativeInitialAliasPreparation) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	legacyCanonicalStorageAllocation(&heap, int(work.nodes), int64(unsafe.Sizeof(legacyNativeInitialAliasNode{})))
	legacyCanonicalStorageAllocation(&heap, int(work.cursors), int64(unsafe.Sizeof(legacyNativeAnyMapCheckpoint{})))
	return heap.bytes, heap.err
}

func (work *legacyNativeInitialAliasPreparation) close() {
	if work != nil {
		*work = legacyNativeInitialAliasPreparation{closed: true}
	}
}

func legacyNativeInitialAliasReplace(current, previous, next, count int64) (int64, error) {
	if current < 0 || previous < 0 || next < 0 || count < 0 || previous != 0 && count > math.MaxInt64/previous || next != 0 && count > math.MaxInt64/next {
		return 0, ErrIntegrity
	}
	removed, added := count*previous, count*next
	if current < removed || current-removed > math.MaxInt64-added {
		return 0, ErrIntegrity
	}
	return current - removed + added, nil
}

func (native *legacyNativeObject) refreshInitialAlias(ctx context.Context, next docRangeHeaderHeapEstimate) error {
	binding := native.initialAlias
	if binding.count == 0 {
		return nil
	}
	input, output := native.metadata.match("input"), native.metadata.match("output")
	if native.metadata != &legacyEventNativeFields || input < 0 || output < 0 {
		return ErrIntegrity
	}
	if native.field != input {
		if native.field == output {
			target := native.destination.Elem().Field(native.metadata.fields[output].index).Interface()
			key, _, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, target)
			if err != nil {
				return err
			}
			if key != binding.output {
				native.initialAlias = legacyNativeInitialAlias{}
			}
		}
		return nil
	}
	value := native.destination.Elem().Field(native.metadata.fields[input].index)
	target := native.destination.Elem().Field(native.metadata.fields[output].index).Interface()
	key, _, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, target)
	if err != nil {
		return err
	}
	if value.IsNil() || reflect.ValueOf(value.Interface()).Pointer() != binding.input || key != binding.output {
		native.initialAlias = legacyNativeInitialAlias{}
		return nil
	}
	heap, err := legacyNativeInitialAliasReplace(native.heaps[output].bytes, native.heaps[input].bytes, next.bytes, binding.count)
	if err != nil {
		return err
	}
	excess, err := legacyNativeInitialAliasReplace(native.backingExcess[output], native.backingExcess[input], 0, binding.count)
	if err != nil {
		return err
	}
	native.heaps[output].bytes, native.backingExcess[output] = heap, excess
	return nil
}

func (native *legacyNativeObject) clearPublishedInitialAlias() {
	if native.initialAlias.count != 0 && native.metadata == &legacyEventNativeFields && native.field == native.metadata.match("output") {
		native.initialAlias = legacyNativeInitialAlias{}
	}
}

func (native *legacyNativeObject) preserveInitialAliasHeap(ctx context.Context) (bool, error) {
	if native.initialAlias.count == 0 || native.metadata != &legacyEventNativeFields || native.field != native.metadata.match("output") {
		return false, nil
	}
	value := native.destination.Elem().Field(native.metadata.fields[native.field].index).Interface()
	key, _, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, value)
	if err != nil {
		return false, err
	}
	return key == native.initialAlias.output, nil
}
