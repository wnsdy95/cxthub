package domain

import (
	"context"
	"reflect"
	"unsafe"
)

func (work *legacyNativeObjectCheckpoint) prepareInitialFields() error {
	var count int
	for index, field := range work.native.metadata.fields {
		if !field.genericMap && !field.interfaceString || work.guards[index].dynamic != 2 {
			continue
		}
		value := work.native.destination.Elem().Field(field.index)
		if field.interfaceString {
			if value.IsNil() {
				continue
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Map && value.Kind() != reflect.Slice || value.IsNil() {
			continue
		}
		if work.native.containers != nil && work.native.containers[index] != nil {
			continue
		}
		count++
	}
	if count == 0 {
		return nil
	}
	for _, parser := range work.native.containers {
		if parser != nil {
			return ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(int64(len(work.guards)), 1)
	planned.allocation(int64(count), int64(unsafe.Sizeof(any(nil))))
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return err
	}
	work.initialSlots = make([]uint8, len(work.guards))
	work.initialValues = make([]any, 0, count)
	for index, field := range work.native.metadata.fields {
		if !field.genericMap && !field.interfaceString || work.guards[index].dynamic != 2 {
			continue
		}
		value := work.native.destination.Elem().Field(field.index)
		if field.interfaceString {
			if value.IsNil() {
				continue
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Map && value.Kind() != reflect.Slice || value.IsNil() {
			continue
		}
		work.initialValues = append(work.initialValues, value.Interface())
		work.initialSlots[index] = uint8(len(work.initialValues))
	}
	work.state.InitialGraphCount = uint8(len(work.initialValues))
	return nil
}

func legacyNativeObjectCheckpointInitialRow(state legacyNativeObjectCheckpointControl, metadata *legacyNativeObjectFields, index int) (bool, error) {
	row := state.Fields[index]
	if row.InitialGraph == 0 {
		return false, nil
	}
	field := metadata.fields[index]
	if state.Model != legacyNativeCheckpointEventModel || !field.genericMap && !field.interfaceString || row.InitialGraph > state.InitialGraphCount || row.Index != index || !row.Present || !row.Initialized || !legacyNativeObjectCheckpointPlainRow(row) || row.Boolean || row.BooleanAlias != 0 || row.Dynamic != 0 || row.Any != nil || row.Nested != nil || row.Block != nil {
		return true, ErrIntegrity
	}
	return true, nil
}

func legacyNativeObjectInitialValuesHeap(values []legacyNativeAnyValue) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(cap(values)), int64(unsafe.Sizeof(legacyNativeAnyValue{})))
	for _, value := range values {
		heap.include(value.heap)
		heap.include(docRangeHeaderHeapEstimate{bytes: value.owner.bytes})
	}
	return heap
}

func (work *legacyNativeObjectCheckpointRestore) restoreInitialGraphs(ctx context.Context) error {
	if work.initialTask == nil {
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		current, err := newLegacyNativeCheckpointGoGraphSetRestore(ctx, *work.state.InitialGraphs, work.binding, work.store, remaining)
		if err != nil {
			return err
		}
		work.initialTask = current
		return nil
	}
	ready, err := work.initialTask.step(ctx)
	if err != nil || !ready {
		return err
	}
	values, err := work.initialTask.take(ctx)
	work.initialTask = nil
	if err != nil {
		return err
	}
	if len(values) != int(work.state.InitialGraphCount) {
		return ErrIntegrity
	}
	heap := legacyNativeObjectInitialValuesHeap(values)
	if heap.err != nil {
		return heap.err
	}
	if err := work.admit(heap.bytes); err != nil {
		return err
	}
	work.initialValues = values
	return nil
}

func (work *legacyNativeObjectCheckpointRestore) restoreInitialField(ctx context.Context) (bool, error) {
	row := work.state.Fields[work.field]
	if row.InitialGraph == 0 {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	index := int(row.InitialGraph) - 1
	if index < 0 || index >= len(work.initialValues) {
		return true, ErrIntegrity
	}
	root := work.initialValues[index]
	if err := root.validateOwnership(); err != nil {
		return true, err
	}
	field := work.native.metadata.fields[work.field]
	value := work.native.destination.Elem().Field(field.index)
	actual := reflect.ValueOf(root.value)
	if !actual.IsValid() || !actual.Type().AssignableTo(value.Type()) || actual.Kind() != reflect.Map && actual.Kind() != reflect.Slice || actual.IsNil() {
		return true, ErrIntegrity
	}
	heap := root.heap
	if field.interfaceString {
		heap.allocation(1, int64(value.Type().Size()))
	}
	if heap.err != nil {
		return true, heap.err
	}
	if err := work.admit(heap.bytes); err != nil {
		return true, err
	}
	value.Set(actual)
	work.native.heaps[work.field], work.native.backingExcess[work.field] = heap, root.excess
	work.field++
	return true, nil
}
