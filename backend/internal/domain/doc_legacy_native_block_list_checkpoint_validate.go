package domain

import (
	"reflect"
	"unsafe"
)

func legacyNativeCheckpointBlockListHashValid(hash ContentHash) bool {
	if len(hash) != len(contentHashPrefix)+64 || hash[:len(contentHashPrefix)] != contentHashPrefix {
		return false
	}
	for ordinal := len(contentHashPrefix); ordinal < len(hash); ordinal++ {
		current := hash[ordinal]
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func legacyNativeCheckpointBlockListTreeValid(tree legacyNativeCheckpointTree, count uint64) bool {
	if count == 0 {
		return tree == (legacyNativeCheckpointTree{})
	}
	if tree.Count != count || count > uint64(MaxFinalizedDocBytes) || !legacyNativeCheckpointBlockListHashValid(tree.Ref.Hash) ||
		tree.Ref.Bytes <= 0 || tree.Ref.Bytes > LegacyCanonicalEncodingBytes || tree.Height > legacyNativeCheckpointTreeHeight {
		return false
	}
	var height uint8
	for remaining := count; remaining > legacyNativeCheckpointPageItems; height++ {
		remaining = (remaining + legacyNativeCheckpointPageItems - 1) / legacyNativeCheckpointPageItems
	}
	return tree.Height == height
}

func (array legacyNativeCheckpointBlockArray) validate(complete bool) error {
	if array.Length < 0 || array.Capacity < array.Length || array.Capacity > MaxFinalizedDocBytes ||
		array.Nil && (array.Length != 0 || array.Capacity != 0) {
		return ErrIntegrity
	}
	if complete {
		if !legacyNativeCheckpointBlockListTreeValid(array.Records, uint64(array.Capacity)) {
			return ErrIntegrity
		}
	} else if array.Records != (legacyNativeCheckpointTree{}) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointBlockListSeedMetadata(list *legacyNativeBlockList) error {
	fields := len(legacyNativeBlockValue{}.heaps)
	if list == nil || legacyContentBlockNativeFields.err != nil || len(legacyContentBlockNativeFields.fields) != fields ||
		cap(list.decoded) > MaxFinalizedDocBytes || cap(list.previous) > MaxFinalizedDocBytes ||
		cap(list.decoded) > int(^uint(0)>>1)/fields || cap(list.previous) > int(^uint(0)>>1)/fields {
		return ErrIntegrity
	}
	if list.paged {
		if list.decoded != nil || list.previous != nil || list.payloads != nil || list.previousHeaps != nil ||
			list.excess != nil || list.previousExtra != nil {
			return ErrIntegrity
		}
		return nil
	}
	length := cap(list.decoded) * fields
	if len(list.payloads) != len(list.excess) || len(list.previousHeaps) != cap(list.previous)*fields ||
		len(list.previousExtra) != cap(list.previous)*fields ||
		list.initialized && len(list.payloads) != length ||
		!list.initialized && (len(list.payloads) != 0 && len(list.payloads) != length || list.seeded < 0 || list.seeded > len(list.payloads)/fields) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointBlockListControlFor(list *legacyNativeBlockList, binding ContentHash, sourceBytes int) (legacyNativeCheckpointBlockListState, error) {
	if list == nil {
		return legacyNativeCheckpointBlockListState{}, ErrIntegrity
	}
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil {
		return legacyNativeCheckpointBlockListState{}, err
	}
	state := legacyNativeCheckpointBlockListState{
		Version: legacyNativeCheckpointVersion, Binding: binding, SourceBytes: sourceBytes, Schema: schema,
		Position: list.position, End: list.end, ParentDepth: list.parentDepth, Maximum: list.maximum,
		Count: list.count, Copied: list.copied, ChildStart: list.childStart, Literal: list.literal, Seeded: list.seeded,
		Phase: list.phase, Prefix: list.prefix, Paged: list.paged, Initialized: list.initialized,
		Decoded:  legacyNativeCheckpointBlockArray{Nil: list.decoded == nil, Length: len(list.decoded), Capacity: cap(list.decoded)},
		Previous: legacyNativeCheckpointBlockArray{Nil: list.previous == nil, Length: len(list.previous), Capacity: cap(list.previous)},
		HasData:  list.data != nil, HasFrame: list.frame != nil,
	}
	if list.data != nil {
		if !list.paged || !list.data.records.valid() {
			return legacyNativeCheckpointBlockListState{}, ErrIntegrity
		}
		state.DataCount = list.data.count
		state.DataRecords.Count = uint64(list.data.records.length())
	}
	if err := legacyNativeCheckpointBlockListValidate(state, binding, sourceBytes, false); err != nil {
		return legacyNativeCheckpointBlockListState{}, err
	}
	if err := legacyNativeCheckpointBlockListSeedMetadata(list); err != nil {
		return legacyNativeCheckpointBlockListState{}, err
	}
	if list.phase != legacyNativeBlocksApply {
		if list.object != nil || list.block != (ContentBlock{}) {
			return legacyNativeCheckpointBlockListState{}, ErrIntegrity
		}
		return state, nil
	}
	object := list.object
	if object == nil || object.metadata != &legacyContentBlockNativeFields || object.destination.Kind() != reflect.Pointer ||
		object.destination.IsNil() || object.destination.Type() != reflect.TypeFor[*ContentBlock]() ||
		object.destination.Pointer() != uintptr(unsafe.Pointer(&list.block)) || object.parentDepth != list.parentDepth+1 ||
		object.position < list.childStart || object.position > object.end || object.end > list.end ||
		len(object.heaps) != len(legacyNativeBlockValue{}.heaps) || len(object.backingExcess) != len(legacyNativeBlockValue{}.excess) {
		return legacyNativeCheckpointBlockListState{}, ErrIntegrity
	}
	if object.prefix {
		if object.end != list.end || list.parentDepth >= 10000 ||
			list.position != list.childStart && (object.phase != legacyNativeObjectDone || list.position != object.position) {
			return legacyNativeCheckpointBlockListState{}, ErrIntegrity
		}
	} else if list.position-list.childStart != len("null") || object.end != list.position || list.literal != len("null") {
		return legacyNativeCheckpointBlockListState{}, ErrIntegrity
	}
	record, err := list.blockRecord()
	if err != nil {
		return legacyNativeCheckpointBlockListState{}, err
	}
	if unsafe.StringData(list.block.Type) == unsafe.StringData(record.block.Type) {
		state.BlockAliases |= 1
	}
	if unsafe.StringData(list.block.Text) == unsafe.StringData(record.block.Text) {
		state.BlockAliases |= 2
	}
	return state, nil
}

func legacyNativeCheckpointBlockListValidate(state legacyNativeCheckpointBlockListState, binding ContentHash, sourceBytes int, complete bool) error {
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil || state.Schema != schema {
		return ErrIntegrity
	}
	if state.Version != legacyNativeCheckpointVersion || !legacyNativeCheckpointBlockListHashValid(binding) || state.Binding != binding ||
		sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || state.SourceBytes != sourceBytes ||
		state.Position < 0 || state.End <= 0 || state.Position > state.End || state.End > sourceBytes ||
		state.ParentDepth < 1 || state.ParentDepth > 10000 || state.Maximum < 0 || state.Maximum > state.End/3 ||
		state.Position < max(0, state.End-3*state.Maximum-2) || state.Count < 0 || state.Count > state.Maximum ||
		state.Copied < 0 || state.Copied > state.Count || state.ChildStart < 0 || state.ChildStart > state.Position ||
		state.Literal < 0 || state.Literal > len("null") || state.Seeded < 0 || state.Seeded > MaxFinalizedDocBytes ||
		state.Phase > legacyNativeBlocksDone || state.DataCount < 0 || state.DataRecords.Count > uint64(MaxFinalizedDocBytes) || state.BlockAliases > 3 {
		return ErrIntegrity
	}
	if err := state.Decoded.validate(complete); err != nil {
		return err
	}
	if err := state.Previous.validate(complete); err != nil {
		return err
	}
	if !state.Initialized && (state.Paged || state.Phase != legacyNativeBlocksStart || state.Decoded.Capacity == 0 || state.Seeded >= state.Decoded.Capacity) {
		return ErrIntegrity
	}
	if state.Phase != legacyNativeBlocksScan && state.Phase != legacyNativeBlocksNull && state.Literal != 0 && state.Literal != len("null") {
		return ErrIntegrity
	}
	if state.Paged {
		if !state.Decoded.Nil || !state.Previous.Nil || !state.Initialized || state.Seeded != 0 || state.Copied != 0 ||
			state.DataCount != state.Count || uint64(state.DataCount) > state.DataRecords.Count {
			return ErrIntegrity
		}
		if state.HasData {
			if complete {
				if !legacyNativeCheckpointBlockListTreeValid(state.DataRecords, state.DataRecords.Count) {
					return ErrIntegrity
				}
			} else if state.DataRecords.Ref != (legacyNativeCheckpointRef{}) || state.DataRecords.Height != 0 {
				return ErrIntegrity
			}
		} else if state.DataCount != 0 || state.DataRecords != (legacyNativeCheckpointTree{}) {
			return ErrIntegrity
		}
	} else if state.HasData || state.DataCount != 0 || state.DataRecords != (legacyNativeCheckpointTree{}) || state.Count > state.Decoded.Capacity {
		return ErrIntegrity
	}
	if !state.Previous.Nil {
		if !state.Initialized || state.Paged || state.Phase != legacyNativeBlocksValue || state.Previous.Capacity == 0 ||
			state.Previous.Length != state.Previous.Capacity || state.Previous.Length != state.Count || state.Copied >= state.Previous.Length ||
			state.Decoded.Capacity <= state.Previous.Capacity || state.Decoded.Capacity > state.Maximum {
			return ErrIntegrity
		}
	}
	if state.Phase < legacyNativeBlocksTail {
		if state.Decoded.Length != state.Decoded.Capacity {
			return ErrIntegrity
		}
	} else {
		if !state.Initialized || !state.Previous.Nil || state.Phase == legacyNativeBlocksDone && !state.Prefix && state.Position != state.End {
			return ErrIntegrity
		}
		if state.Paged {
			if state.Count == 0 && state.DataRecords.Count != 0 || !state.HasData && state.Literal != len("null") ||
				state.HasData && state.Count == 0 && state.Literal != 0 {
				return ErrIntegrity
			}
		} else if state.Decoded.Length != state.Count || state.Count == 0 && state.Decoded.Capacity != 0 ||
			state.Decoded.Nil && state.Literal != len("null") || !state.Decoded.Nil && state.Count == 0 && state.Literal != 0 {
			return ErrIntegrity
		}
		if state.Count == 0 && (state.Copied != 0 || state.ChildStart != 0) {
			return ErrIntegrity
		}
		if state.Count > 0 && (state.ChildStart == 0 || state.ChildStart >= state.Position) {
			return ErrIntegrity
		}
	}
	switch state.Phase {
	case legacyNativeBlocksStart, legacyNativeBlocksFirst:
		if state.Count != 0 || state.Copied != 0 || state.ChildStart != 0 || state.Literal != 0 ||
			state.Phase == legacyNativeBlocksFirst && state.Paged && !state.HasData {
			return ErrIntegrity
		}
	case legacyNativeBlocksValue, legacyNativeBlocksAfter:
		if state.Count == 0 || state.ChildStart == 0 || state.ChildStart >= state.Position || state.Paged && !state.HasData {
			return ErrIntegrity
		}
	case legacyNativeBlocksScan, legacyNativeBlocksApply:
		if !state.Initialized || !state.Previous.Nil || state.Count >= state.Maximum || state.ChildStart == 0 || state.ChildStart >= state.End ||
			state.ChildStart <= max(0, state.End-3*state.Maximum-2) ||
			state.Paged && (!state.HasData || uint64(state.Count) >= state.DataRecords.Count) ||
			!state.Paged && state.Count >= state.Decoded.Capacity ||
			state.Phase == legacyNativeBlocksScan && (state.Literal == len("null") || state.Position-state.ChildStart != state.Literal) {
			return ErrIntegrity
		}
	case legacyNativeBlocksNull:
		if state.Count != 0 || state.Copied != 0 || state.ChildStart != 0 || !state.Previous.Nil || state.Literal == len("null") {
			return ErrIntegrity
		}
	}
	if state.Phase != legacyNativeBlocksApply && state.BlockAliases != 0 {
		return ErrIntegrity
	}
	if complete {
		if state.HasFrame {
			if !legacyNativeCheckpointBlockListHashValid(state.Frame.Hash) || !state.Frame.valid() {
				return ErrIntegrity
			}
		} else if state.Frame != (legacyNativeCheckpointRef{}) {
			return ErrIntegrity
		}
		if state.Phase == legacyNativeBlocksApply {
			if !legacyNativeCheckpointBlockListHashValid(state.Object.Hash) || !state.Object.valid() {
				return ErrIntegrity
			}
		} else if state.Object != (legacyNativeCheckpointRef{}) {
			return ErrIntegrity
		}
	} else if state.Frame != (legacyNativeCheckpointRef{}) || state.Object != (legacyNativeCheckpointRef{}) {
		return ErrIntegrity
	}
	return nil
}
