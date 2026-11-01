package domain

import (
	"context"
	"unsafe"
)

func legacyNativeObjectCheckpointNestedFieldModel(metadata *legacyNativeObjectFields, index int) (uint8, error) {
	if metadata == nil || index < 0 || index >= len(metadata.fields) || !metadata.fields[index].nestedObject {
		return 0, ErrIntegrity
	}
	field := metadata.model.Field(metadata.fields[index].index)
	model := legacyNativeObjectCheckpointNestedModel(field.Type)
	if model == 0 {
		return 0, ErrIntegrity
	}
	return model, nil
}

func legacyNativeObjectCheckpointNestedRow(state legacyNativeObjectCheckpointControl, metadata *legacyNativeObjectFields, index int) (bool, error) {
	row := state.Fields[index]
	if !metadata.fields[index].nestedObject {
		if row.Nested != nil {
			return false, ErrIntegrity
		}
		return false, nil
	}
	if !legacyNativeObjectCheckpointPlainRow(row) || row.Block != nil || row.Boolean || row.BooleanAlias != 0 || row.Dynamic != 0 {
		return true, ErrIntegrity
	}
	active := state.NestedActive && state.Field == index
	if row.Nested == nil {
		if row.Present || active {
			return true, ErrIntegrity
		}
		return true, nil
	}
	model, err := legacyNativeObjectCheckpointNestedFieldModel(metadata, index)
	if err != nil {
		return true, err
	}
	if row.Nested.Parser != (legacyNativeCheckpointRef{}) && !state.HasNestedCache || active && row.Nested.Parser == (legacyNativeCheckpointRef{}) || !row.Initialized && row.Present && !active {
		return true, ErrIntegrity
	}
	return true, row.Nested.validate(model, row.Present, row.Initialized)
}

func (work *legacyNativeObjectCheckpoint) captureNestedField(ctx context.Context) (bool, error) {
	if !work.native.metadata.fields[work.field].nestedObject {
		return false, nil
	}
	guard := work.guards[work.field]
	row := &work.state.Fields[work.field]
	if !guard.present {
		*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0}
		work.field++
		return true, nil
	}
	if work.nestedTask == nil {
		remaining, err := work.remaining()
		if err != nil {
			return true, err
		}
		current, err := newLegacyNativeObjectNestedCheckpoint(ctx, work.native, work.field, work.scanner, work.state.Binding, work.source, work.store, remaining)
		if err != nil {
			return true, err
		}
		work.nestedTask = current
		return true, nil
	}
	ready, err := work.nestedTask.step(ctx)
	if err != nil || !ready {
		return true, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpointNestedField{})))
	if err := work.admit(planned.bytes); err != nil {
		return true, err
	}
	nested, err := work.nestedTask.take(ctx)
	work.nestedTask = nil
	if err != nil {
		return true, err
	}
	*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: true, Nested: &nested}
	work.field++
	return true, nil
}

func (work *legacyNativeObjectCheckpointRestore) restoreNestedField(ctx context.Context) (bool, error) {
	field := work.native.metadata.fields[work.field]
	if !field.nestedObject {
		return false, nil
	}
	row := work.state.Fields[work.field]
	value := work.native.destination.Elem().Field(field.index)
	if row.Nested == nil {
		if row.Initialized {
			var heap docRangeHeaderHeapEstimate
			heap.allocation(1, int64(value.Type().Size()))
			if err := work.admit(heap.bytes); err != nil {
				return true, err
			}
			work.native.heaps[work.field] = heap
		}
		work.field++
		return true, nil
	}
	if work.nestedTask == nil {
		remaining, err := work.remaining()
		if err != nil {
			return true, err
		}
		current, err := newLegacyNativeObjectNestedCheckpointRestore(ctx, *row.Nested, row.Initialized, row.Present, work.binding, work.source, work.store, remaining)
		if err != nil {
			return true, err
		}
		work.nestedTask = current
		return true, nil
	}
	ready, err := work.nestedTask.step(ctx)
	if err != nil || !ready {
		return true, err
	}
	nested, err := work.nestedTask.take(ctx)
	work.nestedTask = nil
	if err != nil {
		return true, err
	}
	if !nested.Value.IsValid() || nested.Value.Type() != value.Type() || nested.Value.IsNil() != !row.Present || nested.Source.authority != work.source.authority {
		return true, ErrIntegrity
	}
	active := work.state.NestedActive && work.state.Field == work.field
	var adopted docRangeHeaderHeapEstimate
	if child := nested.Parser; child != nil {
		if work.native.nested == nil || child.destination.Type() != value.Type() || child.destination.Pointer() != nested.Value.Pointer() || child.metadata != legacyNativeNestedMetadata(value.Type()) || child.parentDepth != work.state.ParentDepth+1 || child.trackPresence {
			return true, ErrIntegrity
		}
		if active {
			if !child.prefix || child.position < work.state.ValueStart || child.position > work.state.ValueEnd || child.end != work.state.ValueEnd || child.fieldLimit != work.native.fieldActionLimit() || nested.Scanner != work.scanner {
				return true, ErrIntegrity
			}
		} else if child.phase != legacyNativeObjectDone || nested.Scanner != (legacyJSONValueScanner{}) {
			return true, ErrIntegrity
		}
		retained, failure := child.retained()
		adopted.include(docRangeHeaderHeapEstimate{bytes: retained, err: failure})
		adopted.allocation(1, int64(child.destination.Type().Size()))
		adopted.allocation(1, int64(child.metadata.model.Size()))
	} else {
		if active {
			return true, ErrIntegrity
		}
		adopted.include(nested.Heap)
	}
	adopted.include(legacyNativeAnyCheckpointSourceHeap(nested.Source))
	if adopted.err != nil {
		return true, adopted.err
	}
	if err := work.admit(adopted.bytes); err != nil {
		return true, err
	}
	value.Set(nested.Value)
	if nested.Parser != nil {
		nested.Parser.mutation = work.native.mutation
		work.native.nested[work.field] = nested.Parser
	}
	work.native.heaps[work.field], work.native.backingExcess[work.field] = nested.Heap, nested.Excess
	work.source = nested.Source
	work.field++
	return true, nil
}
