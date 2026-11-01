package domain

import (
	"context"
	"math"
	"reflect"
	"unsafe"
)

func (state legacyNativeObjectCheckpointControl) containerDispatch(metadata *legacyNativeObjectFields) bool {
	if metadata == nil || state.Phase != legacyNativeObjectApply || state.Field < 0 || state.Field >= len(metadata.fields) || state.SkipValue {
		return false
	}
	field := metadata.fields[state.Field]
	if !field.interfaceString && !field.genericMap || state.ValueFirst != '{' && state.ValueFirst != '[' || field.genericMap && state.ValueFirst != '{' {
		return false
	}
	return state.Position == state.ValueStart && state.ValueEnd == state.End && state.ValueStart < state.ValueEnd
}

func legacyNativeObjectCheckpointAnyGuard(native *legacyNativeObject, index int, guard *legacyNativeObjectCheckpointValueGuard) (bool, error) {
	field := native.metadata.fields[index]
	if !field.genericMap && !field.interfaceString {
		return false, nil
	}
	value := native.destination.Elem().Field(field.index)
	if native.containers != nil {
		if len(native.containers) != len(native.metadata.fields) {
			return true, ErrIntegrity
		}
		guard.container = native.containers[index]
	}
	if guard.container == nil && value.IsNil() || guard.container == nil && field.interfaceString && value.Elem().Type() == reflect.TypeFor[string]() {
		return false, nil
	}
	guard.dynamic = 2
	guard.present = !value.IsNil()
	if field.interfaceString {
		if !guard.present {
			return true, nil
		}
		value = value.Elem()
	}
	guard.anyType = value.Type()
	switch value.Kind() {
	case reflect.Map:
		if value.Type() != reflect.TypeFor[map[string]any]() {
			return true, ErrIntegrity
		}
		guard.anyPointer, guard.anyLength = value.Pointer(), value.Len()
	case reflect.Slice:
		if value.Type() != reflect.TypeFor[[]any]() {
			return true, ErrIntegrity
		}
		guard.anyPointer, guard.anyLength, guard.anyCapacity = value.Pointer(), value.Len(), value.Cap()
	case reflect.String:
		if value.Type() != reflect.TypeFor[string]() {
			return true, ErrIntegrity
		}
		guard.text = value.String()
	case reflect.Float64:
		guard.anyBits = math.Float64bits(value.Float())
	case reflect.Bool:
		guard.boolean = value.Bool()
	default:
		return true, ErrIntegrity
	}
	if guard.heap.bytes == 0 && guard.present && !(field.genericMap && native.containerActive && native.field == index && guard.container != nil) || guard.excess%2 != 0 {
		return true, ErrIntegrity
	}
	return true, nil
}

func legacyNativeObjectCheckpointAnyRow(state legacyNativeObjectCheckpointControl, metadata *legacyNativeObjectFields, index int) (bool, error) {
	row := state.Fields[index]
	field := metadata.fields[index]
	if row.Any == nil {
		if state.ContainerActive && state.Field == index {
			return true, ErrIntegrity
		}
		return false, nil
	}
	if state.Model != legacyNativeCheckpointEventModel || !field.genericMap && !field.interfaceString || !legacyNativeObjectCheckpointPlainRow(row) || row.Block != nil || row.Nested != nil || row.Boolean || row.BooleanAlias != 0 || row.Dynamic != 0 {
		return true, ErrIntegrity
	}
	if row.Any.Parser != (legacyNativeCheckpointRef{}) && !state.HasContainerCache || state.ContainerActive && state.Field == index && row.Any.Parser == (legacyNativeCheckpointRef{}) {
		return true, ErrIntegrity
	}
	if row.Any.Active != (state.ContainerActive && state.Field == index) {
		return true, ErrIntegrity
	}
	return true, row.Any.validate(field.genericMap, row.Initialized)
}

func (work *legacyNativeObjectCheckpoint) captureAnyField(ctx context.Context) (bool, error) {
	guard := work.guards[work.field]
	if guard.dynamic != 2 {
		return false, nil
	}
	if work.anyTask == nil {
		remaining, err := work.remaining()
		if err != nil {
			return true, err
		}
		var parent *legacyNativeObjectCheckpoint
		if work.state.AnyGraphNamespace == 1 {
			parent = work
		}
		current, err := newLegacyNativeObjectAnyCheckpointInObject(ctx, work.native, work.field, work.scanner, work.state.Binding, work.source, work.store, remaining, parent)
		if err != nil {
			return true, err
		}
		work.anyTask = current
		return true, nil
	}
	ready, err := work.anyTask.step(ctx)
	if err != nil || !ready {
		return true, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpointAnyField{})))
	if err := work.admit(planned.bytes); err != nil {
		return true, err
	}
	saved, err := work.anyTask.take(ctx)
	work.anyTask = nil
	if err != nil {
		return true, err
	}
	work.state.Fields[work.field] = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, Any: &saved}
	work.field++
	return true, nil
}

func (work *legacyNativeObjectCheckpointRestore) restoreAnyField(ctx context.Context) (bool, error) {
	row := work.state.Fields[work.field]
	if row.Any == nil {
		return false, nil
	}
	field := work.native.metadata.fields[work.field]
	if work.anyTask == nil {
		remaining, err := work.remaining()
		if err != nil {
			return true, err
		}
		var parent *legacyNativeObjectCheckpointRestore
		if work.state.AnyGraphNamespace == 1 {
			parent = work
		}
		current, err := newLegacyNativeObjectAnyCheckpointRestoreInObject(ctx, *row.Any, field.genericMap, row.Initialized, work.binding, work.source, work.store, remaining, parent)
		if err != nil {
			return true, err
		}
		work.anyTask = current
		return true, nil
	}
	ready, err := work.anyTask.step(ctx)
	if err != nil || !ready {
		return true, err
	}
	if work.state.AnyGraphNamespace == 1 {
		current, ok := work.anyTask.(*legacyNativeObjectAnyCheckpointRestore)
		if !ok || current.sharedParent != work || current.field != work.field {
			return true, ErrConflict
		}
	}
	result, err := work.anyTask.take(ctx)
	work.anyTask = nil
	if err != nil {
		return true, err
	}
	if result.Source.authority != work.source.authority || result.Heap.err != nil || result.Heap.bytes < 0 || result.Excess < 0 || !row.Initialized && result.Heap.bytes != 0 {
		return true, ErrIntegrity
	}
	active := work.state.ContainerActive && work.state.Field == work.field
	var adopted docRangeHeaderHeapEstimate
	value := work.native.destination.Elem().Field(field.index)
	if parser := result.Parser; parser != nil {
		if work.native.containers == nil {
			return true, ErrIntegrity
		}
		if active {
			if parser.from != work.state.ValueStart || parser.end != work.state.ValueEnd || parser.position < parser.from || parser.position > parser.end || parser.parentDepth != work.state.ParentDepth || parser.fieldLimit != work.native.fieldActionLimit() || result.Scanner != work.scanner {
				return true, ErrIntegrity
			}
		} else if !parser.finished || result.Scanner != (legacyJSONValueScanner{}) {
			return true, ErrIntegrity
		}
		parser.retained(&adopted)
		adopted.allocation(1, int64(value.Type().Size()))
		if !field.genericMap && active {
			adopted.include(result.Heap)
		}
	} else {
		if active {
			return true, ErrIntegrity
		}
		adopted.include(result.Heap)
	}
	adopted.include(legacyNativeAnyCheckpointSourceHeap(result.Source))
	combined := docRangeHeaderHeapEstimate{bytes: work.sharedNative}
	if result.GraphBytes < 0 || work.state.AnyGraphNamespace == 0 && result.GraphBytes != 0 || adopted.bytes < result.GraphBytes {
		return true, ErrIntegrity
	}
	combined.include(docRangeHeaderHeapEstimate{bytes: result.GraphBytes})
	if combined.err != nil {
		return true, combined.err
	}
	adopted.bytes -= result.GraphBytes
	if adopted.err != nil {
		return true, adopted.err
	}
	if err := work.admit(adopted.bytes); err != nil {
		return true, err
	}
	if result.Value == nil {
		value.SetZero()
	} else {
		restored := reflect.ValueOf(result.Value)
		if !restored.Type().AssignableTo(value.Type()) {
			return true, ErrIntegrity
		}
		value.Set(restored)
	}
	if !value.IsNil() != row.Present {
		return true, ErrIntegrity
	}
	if result.Parser != nil {
		work.native.containers[work.field] = result.Parser
	}
	work.native.heaps[work.field], work.native.backingExcess[work.field] = result.Heap, result.Excess
	work.sharedNative = combined.bytes
	work.source = result.Source
	work.field++
	return true, nil
}
