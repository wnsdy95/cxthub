package domain

import (
	"context"
	"reflect"
	"unsafe"
)

func legacyNativeObjectCheckpointPlainRow(row legacyNativeObjectCheckpointField) bool {
	return row.Value == (legacyNativeCheckpointScalar{}) && row.Integer == 0 && row.Strings == (legacyNativeCheckpointStrings{}) && row.StringBinding == 0 && row.StringParser == (legacyNativeCheckpointRef{})
}

func legacyNativeObjectCheckpointEventRow(state legacyNativeObjectCheckpointControl, metadata *legacyNativeObjectFields, index int) (bool, error) {
	if state.Fields[index].Event != nil && !metadata.fields[index].eventList {
		return false, ErrIntegrity
	}
	if handled, err := legacyNativeObjectCheckpointInitialRow(state, metadata, index); handled || err != nil {
		return handled, err
	}
	row := state.Fields[index]
	field := metadata.fields[index]
	model := metadata.model.Field(field.index).Type
	if row.Event != nil && !field.eventList {
		return false, ErrIntegrity
	}
	if field.eventList && state.HasEventCache {
		if row.Event == nil || row.Index != index || !legacyNativeObjectCheckpointPlainRow(row) || row.Boolean || row.BooleanAlias != 0 || row.Dynamic != 0 || row.Block != nil || row.Nested != nil || row.Any != nil || row.InitialGraph != 0 {
			return true, ErrIntegrity
		}
		if row.Event.List == (legacyNativeCheckpointRef{}) {
			if state.EventActive && state.Field == index || row.Initialized && !row.Present {
				return true, ErrIntegrity
			}
		} else if !row.Event.List.valid() {
			return true, ErrIntegrity
		}
		return true, nil
	}
	if row.Event != nil {
		return false, ErrIntegrity
	}
	if row.Index != index || row.Dynamic > 1 {
		return false, ErrIntegrity
	}
	if handled, err := legacyNativeObjectCheckpointAnyRow(state, metadata, index); handled || err != nil {
		return handled, err
	}
	if handled, err := legacyNativeObjectCheckpointNestedRow(state, metadata, index); handled || err != nil {
		return handled, err
	}
	if field.blockList {
		if row.Block == nil || !legacyNativeObjectCheckpointPlainRow(row) || row.Boolean || row.BooleanAlias != 0 || row.Dynamic != 0 || row.Block.Initialized != row.Initialized || row.Block.Paged != state.PagedEvents || row.Present == row.Block.Destination.Nil {
			return true, ErrIntegrity
		}
		if row.Block.Parser != (legacyNativeCheckpointRef{}) && !state.HasBlockCache || state.BlockActive && state.Field == index && row.Block.Parser == (legacyNativeCheckpointRef{}) {
			return true, ErrIntegrity
		}
		return true, row.Block.validate(state.SourceBytes)
	}
	if row.Block != nil {
		return false, ErrIntegrity
	}
	if model.Kind() == reflect.Bool || model == reflect.TypeFor[*bool]() {
		if !legacyNativeObjectCheckpointPlainRow(row) || row.Dynamic != 0 || !row.Initialized && (row.Boolean || model.Kind() == reflect.Pointer && row.Present) {
			return true, ErrIntegrity
		}
		if model.Kind() == reflect.Bool {
			if !row.Present || row.BooleanAlias != 0 {
				return true, ErrIntegrity
			}
			return true, nil
		}
		if !row.Present && (row.Boolean || row.BooleanAlias != 0) {
			return true, ErrIntegrity
		}
		if row.BooleanAlias != 0 {
			prior := int(row.BooleanAlias) - 1
			if prior < 0 || prior >= index || metadata.model.Field(metadata.fields[prior].index).Type != model || !state.Fields[prior].Present || state.Fields[prior].Boolean != row.Boolean {
				return true, ErrIntegrity
			}
		}
		return true, nil
	}
	if row.Boolean || row.BooleanAlias != 0 {
		return false, ErrIntegrity
	}
	if field.nestedObject || field.eventList || field.genericMap || field.interfaceString && row.Dynamic == 0 {
		if !legacyNativeObjectCheckpointPlainRow(row) || row.Present || row.Dynamic != 0 {
			return true, ErrIntegrity
		}
		return true, nil
	}
	if row.Dynamic != 0 && (!field.interfaceString || !row.Present || !row.Initialized) {
		return false, ErrIntegrity
	}
	return false, nil
}

func (work *legacyNativeObjectCheckpoint) captureEventField(ctx context.Context) (bool, error) {
	if handled, err := work.captureEventListField(ctx); handled || err != nil {
		return handled, err
	}
	if work.initialSlots != nil && work.initialSlots[work.field] != 0 {
		guard := work.guards[work.field]
		work.state.Fields[work.field] = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, InitialGraph: work.initialSlots[work.field]}
		work.field++
		return true, nil
	}
	if handled, err := work.captureAnyField(ctx); handled || err != nil {
		return handled, err
	}
	if handled, err := work.captureNestedField(ctx); handled || err != nil {
		return handled, err
	}
	field := work.native.metadata.fields[work.field]
	value := work.native.destination.Elem().Field(field.index)
	guard := work.guards[work.field]
	row := &work.state.Fields[work.field]
	if field.blockList {
		if work.blockTask == nil {
			remaining, err := work.remaining()
			if err != nil {
				return true, err
			}
			current, err := newLegacyNativeObjectBlockCheckpoint(ctx, work.native, work.field, work.scanner, work.state.Binding, work.source, work.store, remaining)
			if err != nil {
				return true, err
			}
			work.blockTask = current
			return true, nil
		}
		ready, err := work.blockTask.step(ctx)
		if err != nil || !ready {
			return true, err
		}
		block, err := work.blockTask.take(ctx)
		work.blockTask = nil
		if err != nil {
			return true, err
		}
		*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, Block: &block}
		work.field++
		return true, nil
	}
	if value.Kind() == reflect.Bool || value.Type() == reflect.TypeFor[*bool]() {
		*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, Boolean: guard.boolean}
		if guard.boolPointer != nil {
			for prior := 0; prior < work.field; prior++ {
				if work.guards[prior].boolPointer == guard.boolPointer {
					row.BooleanAlias = uint8(prior + 1)
					break
				}
			}
		}
		work.field++
		return true, nil
	}
	if field.nestedObject || field.eventList || field.genericMap || field.interfaceString && guard.dynamic == 0 {
		*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: false}
		work.field++
		return true, nil
	}
	return false, nil
}

func (work *legacyNativeObjectCheckpointRestore) restoreEventField(ctx context.Context) (bool, error) {
	if handled, err := work.restoreEventListField(ctx); handled || err != nil {
		return handled, err
	}
	if handled, err := work.restoreInitialField(ctx); handled || err != nil {
		return handled, err
	}
	if handled, err := work.restoreAnyField(ctx); handled || err != nil {
		return handled, err
	}
	if handled, err := work.restoreNestedField(ctx); handled || err != nil {
		return handled, err
	}
	field := work.native.metadata.fields[work.field]
	row := work.state.Fields[work.field]
	value := work.native.destination.Elem().Field(field.index)
	if field.blockList {
		if row.Block == nil {
			return true, ErrIntegrity
		}
		if work.blockTask == nil {
			remaining, err := work.remaining()
			if err != nil {
				return true, err
			}
			current, err := newLegacyNativeObjectBlockCheckpointRestore(ctx, *row.Block, work.binding, work.source, work.store, remaining)
			if err != nil {
				return true, err
			}
			work.blockTask = current
			return true, nil
		}
		ready, err := work.blockTask.step(ctx)
		if err != nil || !ready {
			return true, err
		}
		block, err := work.blockTask.take(ctx)
		work.blockTask = nil
		if err != nil {
			return true, err
		}
		if block.Source.authority != work.source.authority {
			return true, ErrIntegrity
		}
		if block.Parser != nil {
			if work.native.blocks == nil {
				return true, ErrIntegrity
			}
			if work.state.BlockActive && work.field == work.state.Field && (block.Parser.phase == legacyNativeBlocksDone || !block.Parser.prefix || block.Parser.parentDepth != work.state.ParentDepth+1 || block.Parser.end != work.state.ValueEnd || block.Parser.position < work.state.ValueStart || block.Parser.maximum != (work.state.ValueEnd-work.state.ValueStart)/3) {
				return true, ErrIntegrity
			}
			if (!work.state.BlockActive || work.field != work.state.Field) && block.Parser.phase != legacyNativeBlocksDone {
				return true, ErrIntegrity
			}
			if block.Parser.frame != nil && *block.Parser.frame != work.scanner {
				return true, ErrIntegrity
			}
			if block.Parser.frame != nil {
				block.Parser.frame = &work.scanner
				if work.eventFrame != nil {
					block.Parser.frame = work.eventFrame
				}
				block.Parser.ownedFrame = false
			}
			block.Parser.mutation = work.native.mutation
			if block.Parser.object != nil {
				block.Parser.object.mutation = work.native.mutation
			}
			work.native.blocks[work.field] = block.Parser
		}
		value.Set(reflect.ValueOf(block.Values))
		work.native.heaps[work.field], work.native.backingExcess[work.field] = block.Heap, block.Excess
		work.source = block.Source
		work.field++
		return true, nil
	}
	if value.Kind() == reflect.Bool || value.Type() == reflect.TypeFor[*bool]() {
		var heap docRangeHeaderHeapEstimate
		if row.Initialized {
			heap.allocation(1, int64(value.Type().Size()))
			if value.Kind() == reflect.Pointer && row.Present {
				heap.allocation(1, int64(unsafe.Sizeof(bool(false))))
			}
		}
		if heap.err != nil {
			return true, heap.err
		}
		if err := work.admit(heap.bytes); err != nil {
			return true, err
		}
		if value.Kind() == reflect.Bool {
			value.SetBool(row.Boolean)
		} else if row.Present {
			if row.BooleanAlias != 0 {
				prior := work.native.metadata.fields[int(row.BooleanAlias)-1]
				pointer, _ := work.native.destination.Elem().Field(prior.index).Interface().(*bool)
				if pointer == nil || *pointer != row.Boolean {
					return true, ErrIntegrity
				}
				value.Set(reflect.ValueOf(pointer))
			} else {
				boolean := row.Boolean
				value.Set(reflect.ValueOf(&boolean))
			}
		}
		work.native.heaps[work.field] = heap
		work.field++
		return true, nil
	}
	if field.nestedObject || field.eventList || field.genericMap || field.interfaceString && row.Dynamic == 0 {
		if row.Initialized {
			work.native.heaps[work.field].allocation(1, int64(value.Type().Size()))
		}
		work.field++
		return true, nil
	}
	return false, nil
}

func (work *legacyNativeObjectCheckpointRestore) retainCheckpointScanner(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.eventFrame != nil || work.native.blocks == nil {
		return nil
	}
	needed := false
	for _, list := range work.native.blocks {
		needed = needed || list != nil && list.frame != nil
	}
	if !needed {
		return nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(work.scanner)))
	if err := work.admit(heap.bytes); err != nil {
		return err
	}
	frame := new(legacyJSONValueScanner)
	*frame = work.scanner
	work.native.checkpointScanner = frame
	for _, list := range work.native.blocks {
		if list != nil && list.frame != nil {
			list.frame = frame
			list.ownedFrame = false
		}
	}
	return nil
}

func (native *legacyNativeObject) bindCheckpointScanner(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if input == nil || frame == nil || native.checkpointScanner == nil {
		return false, ErrIntegrity
	}
	if native.checkpointBinding {
		return native.bindCheckpointEventCursor(input, frame)
	}
	width := int(unsafe.Sizeof(*frame)) + len(native.blocks)*int(unsafe.Sizeof((*legacyNativeBlockList)(nil))) + len(native.events)*int(unsafe.Sizeof((*legacyNativeEventList)(nil)))
	if width > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	if *native.checkpointScanner != *frame {
		return false, ErrIntegrity
	}
	for _, list := range native.blocks {
		if list != nil && list.frame != nil && list.frame != native.checkpointScanner {
			return false, ErrIntegrity
		}
	}
	for _, list := range native.blocks {
		if list != nil && list.frame != nil {
			list.frame = frame
			list.ownedFrame = false
		}
	}
	for _, list := range native.events {
		if list != nil {
			if native.checkpointCursor != nil {
				return false, ErrIntegrity
			}
			native.checkpointCursor = list
		}
	}
	input.workBytes += width
	if native.checkpointCursor != nil {
		native.checkpointBinding = true
		return false, nil
	}
	native.checkpointScanner = nil
	return true, nil
}

func (native *legacyNativeObject) bindCheckpointEventCursor(input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	list := native.checkpointCursor
	if list == nil {
		return false, ErrIntegrity
	}
	width := int(unsafe.Sizeof(*list))
	if list.object != nil {
		width += len(list.object.blocks)*int(unsafe.Sizeof((*legacyNativeBlockList)(nil))) + len(list.object.events)*int(unsafe.Sizeof((*legacyNativeEventList)(nil)))
	}
	if width > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	if *native.checkpointScanner != *frame || list.mutation != native.mutation || list.frame != nil && list.frame != native.checkpointScanner {
		return false, ErrIntegrity
	}
	next, err := legacyNativeEventListCheckpointChild(list)
	if err != nil {
		return false, err
	}
	if list.object != nil {
		for _, blocks := range list.object.blocks {
			if blocks != nil && blocks.frame != nil && blocks.frame != native.checkpointScanner {
				return false, ErrIntegrity
			}
		}
		for _, blocks := range list.object.blocks {
			if blocks != nil && blocks.frame != nil {
				blocks.frame, blocks.ownedFrame = frame, false
			}
		}
	}
	if list.frame != nil {
		list.frame = frame
	}
	input.workBytes += width
	native.checkpointCursor = next
	if next != nil {
		return false, nil
	}
	native.checkpointScanner, native.checkpointBinding = nil, false
	return true, nil
}
