package domain

import (
	"context"
	"reflect"
	"unsafe"
)

func legacyNativeCheckpointStringsShape(values []string) legacyNativeCheckpointStrings {
	return legacyNativeCheckpointStrings{Nil: values == nil, Length: len(values), Capacity: cap(values)}
}

func legacyNativeCheckpointStringsBinding(values []string, list *legacyNativeStringList) uint8 {
	if list == nil || cap(values) == 0 {
		return 0
	}
	if cap(values) == cap(list.decoded) && unsafe.SliceData(values) == unsafe.SliceData(list.decoded) {
		return 1
	}
	if cap(values) == cap(list.previous) && unsafe.SliceData(values) == unsafe.SliceData(list.previous) {
		return 2
	}
	return 0
}

func (work *legacyNativeObjectCheckpoint) writeStringField(ctx context.Context) (bool, error) {
	row := &work.state.Fields[work.field]
	guard := work.guards[work.field]
	value := work.native.destination.Elem().Field(work.native.metadata.fields[work.field].index).Interface().([]string)
	if work.fieldPhase == 0 {
		*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present,
			Strings: legacyNativeCheckpointStringsShape(value), StringBinding: legacyNativeCheckpointStringsBinding(value, guard.list)}
		work.fieldPhase = 1
		return false, nil
	}
	if work.fieldPhase == 1 {
		if guard.list == nil {
			work.fieldPhase = 2
			return false, nil
		}
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return false, err
			}
			current, err := newLegacyNativeCheckpointStringListWriter(ctx, guard.list, work.store, work.source.Bytes, remaining)
			if err != nil {
				return false, err
			}
			work.child = current
			return false, nil
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		row.StringParser, err = work.child.take(ctx)
		work.child = nil
		if err != nil {
			return false, err
		}
		work.fieldPhase = 2
		return false, nil
	}
	if work.fieldPhase != 2 {
		return false, ErrIntegrity
	}
	if row.StringBinding != 0 {
		return true, nil
	}
	if work.strings == nil {
		remaining, err := work.remaining()
		if err != nil {
			return false, err
		}
		current, err := newLegacyNativeCheckpointStringValuesWriter(ctx, value, nil, work.store, remaining)
		if err != nil {
			return false, err
		}
		work.strings = current
		return false, nil
	}
	ready, err := work.strings.step(ctx)
	if err != nil || !ready {
		return false, err
	}
	row.Strings, err = work.strings.take(ctx)
	work.strings = nil
	return err == nil, err
}

func (work *legacyNativeObjectCheckpointRestore) restoreStringField(ctx context.Context) error {
	row := work.state.Fields[work.field]
	value := work.native.destination.Elem().Field(work.native.metadata.fields[work.field].index)
	if work.fieldPhase == 0 {
		if row.StringParser == (legacyNativeCheckpointRef{}) {
			work.fieldPhase = 1
			return nil
		}
		if work.list == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointStringListRestore(ctx, row.StringParser, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.list = current
			return nil
		}
		ready, err := work.list.step(ctx)
		if err != nil || !ready {
			return err
		}
		list, err := work.list.take(ctx)
		work.list = nil
		if err != nil {
			return err
		}
		if work.state.ListActive && work.state.Field == work.field && (list.position < work.state.ValueStart || list.end != work.state.ValueEnd || !list.prefix || list.phase == legacyNativeStringsDone) {
			return ErrIntegrity
		}
		if (!work.state.ListActive || work.state.Field != work.field) && list.phase != legacyNativeStringsDone {
			return ErrIntegrity
		}
		if list.phase == legacyNativeStringsDone {
			if row.Strings.Nil != (list.decoded == nil) || row.Strings.Length != len(list.decoded) || row.Strings.Capacity != cap(list.decoded) || cap(list.decoded) > 0 && row.StringBinding != 1 {
				return ErrIntegrity
			}
			list.owned = cap(list.decoded) > 0
			list.added = list.payload
		}
		work.native.lists[work.field] = list
		work.fieldPhase = 1
		return nil
	}
	if work.fieldPhase == 1 {
		if row.StringBinding != 0 {
			list := work.native.lists[work.field]
			if list == nil {
				return ErrIntegrity
			}
			values := list.decoded
			if row.StringBinding == 2 {
				values = list.previous
			}
			if row.Strings.Nil != (values == nil) || row.Strings.Capacity != cap(values) {
				return ErrIntegrity
			}
			value.Set(reflect.ValueOf(values[:row.Strings.Length]))
			work.fieldPhase = 2
			work.stringHeap.allocation(1, int64(value.Type().Size()))
			work.stringHeap.include(legacyNativeStringsBacking(cap(values)))
			return nil
		}
		if work.strings == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointStringValuesRestore(ctx, row.Strings, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.strings = current
			return nil
		}
		ready, err := work.strings.step(ctx)
		if err != nil || !ready {
			return err
		}
		current, err := work.strings.take(ctx)
		work.strings = nil
		if err != nil {
			return err
		}
		value.Set(reflect.ValueOf(current.Values))
		if row.Initialized {
			heap := current.Payload
			heap.allocation(1, int64(value.Type().Size()))
			heap.include(legacyNativeStringsBacking(cap(current.Values)))
			if heap.err != nil {
				return heap.err
			}
			work.native.heaps[work.field] = heap
		}
		work.finishStringField()
		return nil
	}
	if work.fieldPhase != 2 {
		return ErrIntegrity
	}
	list := work.native.lists[work.field]
	capacities := list.capacities
	if row.StringBinding == 2 {
		capacities = list.previousCaps
	}
	if len(capacities) != value.Cap() {
		return ErrIntegrity
	}
	limit := min(len(capacities), work.stringHeapIndex+1024)
	for work.stringHeapIndex < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		work.stringHeap.allocation(int64(capacities[work.stringHeapIndex]), 1)
		work.stringHeapIndex++
	}
	if work.stringHeap.err != nil {
		return work.stringHeap.err
	}
	if work.stringHeapIndex == len(capacities) {
		if row.Initialized {
			work.native.heaps[work.field] = work.stringHeap
		}
		work.finishStringField()
	}
	return nil
}

func (work *legacyNativeObjectCheckpointRestore) finishStringField() {
	if work.native.lists != nil {
		list := work.native.lists[work.field]
		if list != nil && list.phase == legacyNativeStringsDone {
			list.owned, list.previousOwned = false, false
			list.added = docRangeHeaderHeapEstimate{}
		}
	}
	work.field++
	work.fieldPhase = 0
	work.stringHeapIndex = 0
	work.stringHeap = docRangeHeaderHeapEstimate{}
}
