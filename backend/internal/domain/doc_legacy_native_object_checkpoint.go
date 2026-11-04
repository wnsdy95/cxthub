package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"unsafe"
)

func legacyNativeCheckpointObjectModel(model uint8) *legacyNativeObjectFields {
	if model != legacyNativeCheckpointEnvelopeModel && model != legacyNativeCheckpointEventModel && model != legacyNativeCheckpointBlockModel && model != legacyNativeCheckpointLockedModel && model != legacyNativeCheckpointProviderModel {
		return nil
	}
	return legacyNativeCheckpointModelMetadata(model)
}

func legacyNativeObjectCheckpointControlFor(native *legacyNativeObject, model uint8, binding ContentHash, sourceBytes int) legacyNativeObjectCheckpointControl {
	schema, _ := legacyNativeCheckpointModelSchema(native.metadata)
	var destinationPresence eventFieldPresence
	if model == legacyNativeCheckpointEventModel {
		destinationPresence = native.destination.Interface().(*wire).fieldPresence
	}
	version := uint8(3)
	if native.events != nil {
		version = 4
	}
	return legacyNativeObjectCheckpointControl{Version: version, Binding: binding, SourceBytes: sourceBytes, Model: model,
		Schema:      schema,
		ParentDepth: native.parentDepth, TrackPresence: native.trackPresence, Presence: native.presence, PagedEvents: native.pagedEvents,
		DeferTypeErrors: native.deferTypeErrors, SkipValue: native.skipValue, FieldLimit: native.fieldLimit, Prefix: native.prefix,
		Position: native.position, End: native.end, ValueStart: native.valueStart, ValueEnd: native.valueEnd, Field: native.field,
		Phase: native.phase, ValueFirst: native.valueFirst, KeyStarted: native.keyStarted, KeyOverflow: native.keyOverflow,
		ValueStarted: native.valueStarted, Key: native.key, RawCapacity: cap(native.raw), HasScalar: native.scalar != nil,
		ContainerActive: native.containerActive, HasContainerCache: native.containers != nil, AnyGraphNamespace: legacyNativeObjectAnyGraphNamespace(native), HasFloatNumber: native.number != nil,
		ListActive: native.listActive, HasStringCache: native.lists != nil, BlockActive: native.blockActive, HasBlockCache: native.blocks != nil, EventActive: native.eventActive, HasEventCache: native.events != nil, NestedActive: native.nestedActive, HasNestedCache: native.nested != nil, DestinationPresence: destinationPresence}
}

func (state legacyNativeObjectCheckpointControl) validate(model uint8, binding ContentHash, sourceBytes int, complete bool) error {
	metadata := legacyNativeCheckpointObjectModel(model)
	schema, err := legacyNativeCheckpointModelSchema(metadata)
	if err != nil || state.Schema != schema {
		return ErrIntegrity
	}
	if metadata == nil || state.Version != 3 && state.Version != 4 || state.Version == 4 && !state.HasEventCache || state.Version == 3 && state.HasEventCache || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || state.Binding != binding || state.Model != model ||
		sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || state.SourceBytes != sourceBytes || state.ParentDepth < 1 || state.ParentDepth > 10001 ||
		state.Position < 0 || state.End <= 0 || state.Position > state.End || state.End > sourceBytes || state.ValueStart < 0 || state.ValueStart > state.End ||
		state.ValueEnd < 0 || state.ValueEnd > state.End || state.Field < -1 || state.Field >= len(metadata.fields) || state.Phase > legacyNativeObjectDone ||
		state.FieldLimit < 0 || len(state.Key) > metadata.keyBytes || state.RawCapacity < 0 || state.RawCapacity > metadata.rawBytes ||
		state.Presence & ^(eventV2PresenceMask|eventFieldLocked) != 0 || !state.TrackPresence && state.Presence != 0 {
		return ErrIntegrity
	}
	if state.ParentDepth == 10001 && (model != legacyNativeCheckpointBlockModel && model != legacyNativeCheckpointEventModel || state.Prefix || state.Field != -1 || state.Phase != legacyNativeObjectStart && state.Phase != legacyNativeObjectDone) {
		return ErrIntegrity
	}
	if state.DestinationPresence & ^(eventV2PresenceMask|eventFieldLocked) != 0 || model != legacyNativeCheckpointEventModel && state.DestinationPresence != 0 {
		return ErrIntegrity
	}
	if state.ValueFirst != 0 && state.ValueFirst != '"' && state.ValueFirst != 't' && state.ValueFirst != 'f' && state.ValueFirst != 'n' && state.ValueFirst != '-' &&
		(state.ValueFirst < '0' || state.ValueFirst > '9') && state.ValueFirst != '[' && state.ValueFirst != '{' {
		return ErrIntegrity
	}
	if state.Phase == legacyNativeObjectApply && state.ValueStarted && !state.stringListDispatch(metadata) && !state.blockListDispatch(metadata) && !state.eventListDispatch(metadata) && !state.nestedDispatch(metadata) && !state.containerDispatch(metadata) && (state.ValueEnd < state.ValueStart || state.Position != state.ValueEnd) {
		return ErrIntegrity
	}
	if state.HasScalar && (state.Phase != legacyNativeObjectApply || state.Field < 0 || !metadata.fields[state.Field].scalarString && !metadata.fields[state.Field].interfaceString || state.ValueFirst != '"' || state.SkipValue) {
		return ErrIntegrity
	}
	if state.ListActive && (!state.HasStringCache || state.HasScalar || state.Phase != legacyNativeObjectApply || state.Field < 0 || !metadata.fields[state.Field].stringList || state.SkipValue || state.ValueFirst != '[' && state.ValueFirst != 'n') {
		return ErrIntegrity
	}
	if state.stringListDispatch(metadata) && !state.ListActive && (state.Position != state.ValueStart || state.ValueEnd != state.End) {
		return ErrIntegrity
	}
	if state.BlockActive && (!state.HasBlockCache || state.HasScalar || !state.blockListDispatch(metadata)) {
		return ErrIntegrity
	}
	if state.blockListDispatch(metadata) && (state.Position != state.ValueStart || state.ValueEnd != state.End || state.BlockActive && state.ValueStart >= state.ValueEnd) {
		return ErrIntegrity
	}
	if state.HasEventCache && (model != legacyNativeCheckpointEventModel || !state.PagedEvents) || state.EventActive && (!state.HasEventCache || state.HasScalar || !state.eventListDispatch(metadata) || state.BlockActive || state.NestedActive || state.ContainerActive || state.ListActive) {
		return ErrIntegrity
	}
	if state.eventListDispatch(metadata) && (state.Position != state.ValueStart || state.ValueEnd != state.End || state.EventActive && state.ValueStart >= state.ValueEnd) {
		return ErrIntegrity
	}
	if state.NestedActive && (!state.HasNestedCache || state.HasScalar || !state.nestedDispatch(metadata)) {
		return ErrIntegrity
	}
	if state.nestedDispatch(metadata) && (state.Position != state.ValueStart || state.ValueEnd != state.End || state.ValueStart >= state.ValueEnd) {
		return ErrIntegrity
	}
	if state.HasNestedCache && model != legacyNativeCheckpointEventModel {
		return ErrIntegrity
	}
	if state.HasContainerCache && model != legacyNativeCheckpointEventModel || state.ContainerActive && (!state.HasContainerCache || !state.containerDispatch(metadata) || state.HasScalar || state.BlockActive || state.NestedActive || state.ListActive) {
		return ErrIntegrity
	}
	if state.AnyGraphNamespace > 1 || state.AnyGraphNamespace != 0 && (model != legacyNativeCheckpointEventModel || !state.HasContainerCache || state.InitialGraphCount != 0) {
		return ErrIntegrity
	}
	if state.HasFloatNumber && (model != legacyNativeCheckpointEventModel || state.Phase != legacyNativeObjectApply || state.Field < 0 || !metadata.fields[state.Field].interfaceString || state.SkipValue || state.HasScalar || state.ContainerActive || state.ListActive || state.BlockActive || state.NestedActive || state.Number != (legacyNativeCheckpointRef{}) || state.ValueEnd-state.ValueStart <= LegacyCanonicalEncodingBytes || state.ValueFirst != '-' && (state.ValueFirst < '0' || state.ValueFirst > '9')) {
		return ErrIntegrity
	}
	if !state.HasFloatNumber && state.FloatNumber != nil || complete && state.HasFloatNumber && (state.FloatNumber == nil || !state.FloatNumber.valid()) {
		return ErrIntegrity
	}
	if state.InitialGraphCount > 32 || state.InitialGraphCount != 0 && model != legacyNativeCheckpointEventModel || state.InitialGraphCount == 0 && state.InitialGraphs != nil || complete && state.InitialGraphCount != 0 && (state.InitialGraphs == nil || !state.InitialGraphs.valid()) {
		return ErrIntegrity
	}
	if state.InitialGraphCount != 0 && state.ContainerActive {
		return ErrIntegrity
	}
	if complete {
		if len(state.Fields) != len(metadata.fields) || !state.Scanner.valid() {
			return ErrIntegrity
		}
		for index, row := range state.Fields {
			fieldType := metadata.model.Field(metadata.fields[index].index).Type
			handled, err := legacyNativeObjectCheckpointEventRow(state, metadata, index)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
			if fieldType.Kind() == reflect.Int {
				if row.Index != index || !row.Present || !row.Initialized && row.Integer != 0 || row.Value != (legacyNativeCheckpointScalar{}) || row.Strings != (legacyNativeCheckpointStrings{}) || row.StringBinding != 0 || row.StringParser != (legacyNativeCheckpointRef{}) || reflect.New(fieldType).Elem().OverflowInt(row.Integer) {
					return ErrIntegrity
				}
				continue
			}
			if metadata.fields[index].stringList {
				if row.Index != index || row.Integer != 0 || row.Value != (legacyNativeCheckpointScalar{}) || row.Present == row.Strings.Nil || row.StringBinding > 2 || row.StringBinding != 0 && row.StringParser == (legacyNativeCheckpointRef{}) || row.StringParser != (legacyNativeCheckpointRef{}) && (!state.HasStringCache || !row.StringParser.valid()) || state.ListActive && state.Field == index && row.StringParser == (legacyNativeCheckpointRef{}) || !row.Initialized && !row.Strings.Nil {
					return ErrIntegrity
				}
				if err := row.Strings.validate(row.StringBinding == 0); err != nil {
					return err
				}
				continue
			}
			if row.Integer != 0 || row.Strings != (legacyNativeCheckpointStrings{}) || row.StringBinding != 0 || row.StringParser != (legacyNativeCheckpointRef{}) {
				return ErrIntegrity
			}
			if row.Index != index || fieldType.Kind() == reflect.String && !row.Present || !row.Initialized && (row.Value != (legacyNativeCheckpointScalar{}) || fieldType.Kind() == reflect.Pointer && row.Present) {
				return ErrIntegrity
			}
			if !row.Initialized || !row.Present {
				if row.Value != (legacyNativeCheckpointScalar{}) {
					return ErrIntegrity
				}
				continue
			}
			if err := row.Value.State.validate(sourceBytes); err != nil {
				return err
			}
			value := row.Value.State
			if value.Position != 0 || value.End != 0 || !value.Present || value.Expanded || value.Maximum != int(legacyNativeValueCheckpointPayloadLimit()) || row.Value.Text.Bytes != value.Bytes {
				return ErrIntegrity
			}
		}
		var initialSlots uint32
		parserCount := 0
		for _, row := range state.Fields {
			if row.Any != nil && row.Any.Parser != (legacyNativeCheckpointRef{}) {
				parserCount++
			}
			if state.InitialGraphCount != 0 && row.Any != nil && row.Any.Parser != (legacyNativeCheckpointRef{}) {
				return ErrIntegrity
			}
			if row.InitialGraph != 0 {
				if row.InitialGraph > state.InitialGraphCount || initialSlots&(uint32(1)<<(row.InitialGraph-1)) != 0 {
					return ErrIntegrity
				}
				initialSlots |= uint32(1) << (row.InitialGraph - 1)
			}
		}
		if initialSlots != uint32((uint64(1)<<state.InitialGraphCount)-1) {
			return ErrIntegrity
		}
		if state.AnyGraphNamespace != 0 && parserCount < 2 {
			return ErrIntegrity
		}
		if state.HasScalar {
			if err := state.Scalar.State.validate(sourceBytes); err != nil {
				return err
			}
			if state.Scalar.State.Position < state.ValueStart+1 || state.Scalar.State.End != state.ValueEnd-1 {
				return ErrIntegrity
			}
		} else if state.Scalar != (legacyNativeCheckpointScalar{}) {
			return ErrIntegrity
		}
		if state.Number != (legacyNativeCheckpointRef{}) && (!state.Number.valid() || state.Model != legacyNativeCheckpointProviderModel || state.Phase != legacyNativeObjectApply || state.Field < 0 || !metadata.fields[state.Field].typedNumber || state.SkipValue || state.HasScalar) {
			return ErrIntegrity
		}
		if state.Error != (legacyNativeCheckpointRef{}) && (!state.Error.valid() || !state.DeferTypeErrors) {
			return ErrIntegrity
		}
	}
	return nil
}

func (state legacyNativeObjectCheckpointControl) stringListDispatch(metadata *legacyNativeObjectFields) bool {
	return metadata != nil && state.Phase == legacyNativeObjectApply && state.Field >= 0 && state.Field < len(metadata.fields) && metadata.fields[state.Field].stringList && !state.SkipValue && (state.ValueFirst == '[' || state.ValueFirst == 'n')
}

func (state legacyNativeObjectCheckpointControl) blockListDispatch(metadata *legacyNativeObjectFields) bool {
	return metadata != nil && state.Phase == legacyNativeObjectApply && state.Field >= 0 && state.Field < len(metadata.fields) && metadata.fields[state.Field].blockList && !state.SkipValue && (state.ValueFirst == '[' || state.ValueFirst == 'n')
}

func (state legacyNativeObjectCheckpointControl) eventListDispatch(metadata *legacyNativeObjectFields) bool {
	return metadata != nil && state.PagedEvents && state.Phase == legacyNativeObjectApply && state.Field >= 0 && state.Field < len(metadata.fields) && metadata.fields[state.Field].eventList && !state.SkipValue && (state.ValueFirst == '[' || state.ValueFirst == 'n')
}

func (state legacyNativeObjectCheckpointControl) nestedDispatch(metadata *legacyNativeObjectFields) bool {
	return metadata != nil && state.Phase == legacyNativeObjectApply && state.Field >= 0 && state.Field < len(metadata.fields) && metadata.fields[state.Field].nestedObject && !state.SkipValue && state.ValueFirst == '{'
}

func legacyNativeObjectCheckpointScanner(state legacyNativeObjectCheckpointControl, scanner *legacyJSONValueScanner) error {
	if scanner == nil || scanner.invalid || scanner.key && state.Phase != legacyNativeObjectValue && !state.BlockActive && !state.EventActive && !state.NestedActive && !state.ContainerActive {
		return ErrIntegrity
	}
	if state.Phase == legacyNativeObjectColon || state.Phase == legacyNativeObjectApply && !state.stringListDispatch(legacyNativeCheckpointObjectModel(state.Model)) && !state.blockListDispatch(legacyNativeCheckpointObjectModel(state.Model)) && !state.eventListDispatch(legacyNativeCheckpointObjectModel(state.Model)) && !state.nestedDispatch(legacyNativeCheckpointObjectModel(state.Model)) && !state.containerDispatch(legacyNativeCheckpointObjectModel(state.Model)) {
		if !scanner.complete {
			return ErrIntegrity
		}
	}
	if (state.Phase == legacyNativeObjectKey || state.Phase == legacyNativeObjectValue) && !scanner.complete && scanner.parents != state.ParentDepth {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeObjectCheckpointValueGuard struct {
	text            string
	pointer         *json.Number
	heap            docRangeHeaderHeapEstimate
	excess          int64
	present         bool
	integer         int64
	stringsData     uintptr
	stringsLength   int
	stringsCapacity int
	list            *legacyNativeStringList
	boolean         bool
	boolPointer     *bool
	dynamic         uint8
	block           *legacyNativeBlockList
	nested          *legacyNativeObject
	objectPointer   uintptr
	container       *legacyNativeAny
	event           *legacyNativeEventList
	anyType         reflect.Type
	anyPointer      uintptr
	anyLength       int
	anyCapacity     int
	anyBits         uint64
}

func legacyNativeObjectCheckpointFieldValue(native *legacyNativeObject, index int) (legacyNativeObjectCheckpointValueGuard, error) {
	if native == nil || index < 0 || index >= len(native.metadata.fields) || len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) {
		return legacyNativeObjectCheckpointValueGuard{}, ErrIntegrity
	}
	value := native.destination.Elem().Field(native.metadata.fields[index].index)
	result := legacyNativeObjectCheckpointValueGuard{heap: native.heaps[index], excess: native.backingExcess[index], present: true}
	if result.heap.err != nil || result.heap.bytes < 0 || result.excess < 0 {
		return result, ErrIntegrity
	}
	if handled, err := legacyNativeObjectCheckpointAnyGuard(native, index, &result); handled || err != nil {
		return result, err
	}
	if native.metadata.fields[index].eventList && native.events != nil {
		if !native.pagedEvents || value.Cap() != 0 {
			return result, ErrDocRangeIndexPending
		}
		result.present, result.event = !value.IsNil(), native.events[index]
		if result.event == nil && native.eventActive && native.field == index || result.excess > result.heap.bytes {
			return result, ErrIntegrity
		}
		return result, nil
	}
	if native.metadata.fields[index].blockList {
		result.stringsData, result.stringsLength, result.stringsCapacity = value.Pointer(), value.Len(), value.Cap()
		result.present = !value.IsNil()
		if native.blocks != nil {
			result.block = native.blocks[index]
		}
		if result.heap.bytes == 0 && result.present || result.excess%2 != 0 {
			return result, ErrIntegrity
		}
		return result, nil
	}
	if native.metadata.fields[index].nestedObject {
		result.present, result.objectPointer = !value.IsNil(), value.Pointer()
		if native.nested != nil {
			result.nested = native.nested[index]
		}
		if result.excess%2 != 0 || !result.present && (result.nested != nil || result.excess != 0) || result.present && result.heap.bytes == 0 && (result.nested == nil || !native.nestedActive || native.field != index) {
			return result, ErrIntegrity
		}
		return result, nil
	}
	if value.Kind() == reflect.Bool || value.Type() == reflect.TypeFor[*bool]() {
		if value.Kind() == reflect.Bool {
			result.boolean = value.Bool()
		} else {
			result.boolPointer, _ = value.Interface().(*bool)
			result.present = result.boolPointer != nil
			if result.present {
				result.boolean = *result.boolPointer
			}
		}
		if result.excess != 0 || result.heap.bytes == 0 && (result.boolean || result.boolPointer != nil) {
			return result, ErrIntegrity
		}
		return result, nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			result.present = false
			if result.excess != 0 {
				return result, ErrIntegrity
			}
			return result, nil
		}
		if value.Elem().Kind() != reflect.String || value.Elem().Type() != reflect.TypeFor[string]() || result.heap.bytes == 0 {
			return result, ErrIntegrity
		}
		result.dynamic = 1
		result.text = value.Elem().String()
	} else if native.metadata.fields[index].nestedObject || native.metadata.fields[index].eventList || native.metadata.fields[index].genericMap {
		if !value.IsNil() || result.excess != 0 {
			return result, ErrIntegrity
		}
		result.present = false
		return result, nil
	}
	if value.Kind() == reflect.Interface {
	} else if value.Kind() == reflect.String {
		result.text = value.String()
	} else if value.Kind() == reflect.Int {
		result.integer = value.Int()
		if result.heap.err != nil || result.heap.bytes < 0 || result.excess != 0 || result.heap.bytes == 0 && result.integer != 0 {
			return result, ErrIntegrity
		}
		return result, nil
	} else if native.metadata.fields[index].stringList {
		result.stringsData, result.stringsLength, result.stringsCapacity = value.Pointer(), value.Len(), value.Cap()
		result.present = !value.IsNil()
		if native.lists != nil {
			result.list = native.lists[index]
		}
		if result.heap.err != nil || result.heap.bytes < 0 || result.excess != 0 || result.heap.bytes == 0 && result.present {
			return result, ErrIntegrity
		}
		return result, nil
	} else if value.Type() == reflect.TypeFor[*json.Number]() {
		result.pointer, _ = value.Interface().(*json.Number)
		result.present = result.pointer != nil
		if result.present {
			result.text = string(*result.pointer)
		}
	} else {
		return result, ErrIntegrity
	}
	if result.heap.err != nil || result.heap.bytes < 0 || result.excess < 0 || result.excess%2 != 0 || uint64(len(result.text)) > legacyNativeValueCheckpointPayloadLimit() ||
		result.excess/2 > int64(legacyNativeValueCheckpointPayloadLimit())*2+64-int64(len(result.text)) || result.heap.bytes == 0 && (result.text != "" || result.pointer != nil || result.excess != 0) {
		return result, ErrIntegrity
	}
	return result, nil
}

type legacyNativeObjectCheckpoint struct {
	capture       *legacyNormalizationCapture
	native        *legacyNativeObject
	scanner       *legacyJSONValueScanner
	scannerShape  legacyJSONValueScanner
	shape         legacyNativeObjectCheckpointControl
	state         legacyNativeObjectCheckpointControl
	guards        []legacyNativeObjectCheckpointValueGuard
	scalarSource  *legacyNativeScalar
	numberSource  *legacyNativeTypedNumber
	floatSource   *legacyNativeNumberValue
	errorSource   *json.UnmarshalTypeError
	source        LegacyByteSource
	store         legacyNativeCheckpointIO
	text          *legacyNativeCheckpointTextWriter
	scalar        legacyNativeAnyCheckpointTask[legacyNativeCheckpointScalar]
	child         legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef]
	blockTask     legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointBlockField]
	nestedTask    legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointNestedField]
	anyTask       legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointAnyField]
	anyGraph      *legacyNativeCheckpointGraphIdentity
	floatTask     legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef]
	initialTask   legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef]
	resourceTask  *legacyNativeResourcePreparation
	eventTask     *legacyNativeEventListCheckpoint
	eventOwner    *legacyNativeEventListCheckpoint
	eventSource   int64
	initialSlots  []uint8
	initialValues []any
	strings       legacyNativeAnyCheckpointTask[legacyNativeCheckpointStrings]
	fieldPhase    uint8
	root          legacyNativeCheckpointRef
	field         int
	phase         uint8
	budget        int64
	ready         bool
	closed        bool
	failure       error
}

func newLegacyNativeObjectCheckpoint(ctx context.Context, native *legacyNativeObject, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectCheckpoint, error) {
	return newLegacyNativeObjectCheckpointForEvents(ctx, native, scanner, binding, source, store, budget, nil)
}

func newLegacyNativeObjectCheckpointForEvents(ctx context.Context, native *legacyNativeObject, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64, owner *legacyNativeEventListCheckpoint) (*legacyNativeObjectCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if native == nil || scanner == nil || store.Put == nil || budget <= 0 || len(binding) != len(contentHashPrefix)+64 {
		return nil, ErrIntegrity
	}
	var captureHeap docRangeHeaderHeapEstimate
	captureHeap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	if captureHeap.err != nil {
		return nil, captureHeap.err
	}
	if captureHeap.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	capture, err := native.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	releaseCapture := true
	defer func() {
		if releaseCapture {
			capture.release()
		}
	}()
	model, err := legacyNativeCheckpointModel(native.metadata)
	if err != nil || legacyNativeCheckpointObjectModel(model) == nil {
		return nil, ErrIntegrity
	}
	if !native.destination.IsValid() || native.destination.Kind() != reflect.Pointer || native.destination.IsNil() || native.destination.Elem().Type() != native.metadata.model || cap(native.key) != native.metadata.keyBytes {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return nil, err
	}
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(source.Parts[index].Hash) != len(contentHashPrefix)+64 {
			return nil, ErrIntegrity
		}
		if err := legacyNativeAnyCheckpointSourcePart(source, index); err != nil {
			return nil, err
		}
	}
	state := legacyNativeObjectCheckpointControlFor(native, model, binding, source.Bytes)
	if err := state.validate(model, binding, source.Bytes, false); err != nil {
		return nil, err
	}
	if err := legacyNativeObjectCheckpointScanner(state, scanner); err != nil {
		return nil, err
	}
	if native.checkpointScanner != nil && *native.checkpointScanner != *scanner {
		return nil, ErrIntegrity
	}
	if native.lists != nil && len(native.lists) != len(native.metadata.fields) || native.blocks != nil && len(native.blocks) != len(native.metadata.fields) || native.nested != nil && len(native.nested) != len(native.metadata.fields) || native.containers != nil && len(native.containers) != len(native.metadata.fields) || native.events != nil && len(native.events) != len(native.metadata.fields) {
		return nil, ErrIntegrity
	}
	if native.events != nil && !native.pagedEvents {
		return nil, ErrDocRangeIndexPending
	}
	if owner != nil {
		if err := owner.checkObjectSource(native, scanner, binding, source); err != nil {
			return nil, err
		}
	}
	for index, list := range native.events {
		if list != nil && (!native.metadata.fields[index].eventList || list.mutation != native.mutation) {
			return nil, ErrIntegrity
		}
	}
	for index, list := range native.lists {
		if list != nil && !native.metadata.fields[index].stringList {
			return nil, ErrIntegrity
		}
	}
	for index, list := range native.blocks {
		if list != nil && !native.metadata.fields[index].blockList {
			return nil, ErrIntegrity
		}
	}
	for index, child := range native.nested {
		if child != nil && !native.metadata.fields[index].nestedObject {
			return nil, ErrIntegrity
		}
	}
	for index, parser := range native.containers {
		if parser != nil && !native.metadata.fields[index].genericMap && !native.metadata.fields[index].interfaceString {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	retained := int64(0)
	if owner == nil {
		retained, err = native.retained()
	}
	planned.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(*capture)))
	planned.allocation(int64(len(native.metadata.fields)), int64(unsafe.Sizeof(legacyNativeObjectCheckpointValueGuard{})+unsafe.Sizeof(legacyNativeObjectCheckpointField{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	if state.AnyGraphNamespace != 0 {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphIdentity{})))
	}
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeObjectCheckpoint{capture: capture, native: native, scanner: scanner, scannerShape: *scanner, source: source, store: store, state: state, budget: budget, scalarSource: native.scalar, numberSource: native.typedNumber, floatSource: native.number, errorSource: native.typeError, eventOwner: owner, eventSource: retained}
	releaseCapture = false
	work.shape = state
	work.shape.Key = bytes.Clone(state.Key)
	work.state.Key = bytes.Clone(state.Key)
	work.state.Fields = make(legacyNativeObjectCheckpointFields, len(native.metadata.fields))
	work.guards = make([]legacyNativeObjectCheckpointValueGuard, len(native.metadata.fields))
	if state.AnyGraphNamespace != 0 {
		work.anyGraph = newLegacyNativeCheckpointGraphIdentity()
	}
	for index := range work.guards {
		guard, err := legacyNativeObjectCheckpointFieldValue(native, index)
		if err != nil {
			work.close()
			return nil, err
		}
		work.guards[index] = guard
	}
	if err := work.prepareInitialFields(); err != nil {
		work.close()
		return nil, err
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeObjectCheckpoint) checkSource() error {
	if work.eventOwner != nil {
		if err := work.eventOwner.checkObjectSource(work.native, work.scanner, work.state.Binding, work.source); err != nil {
			return err
		}
	}
	if work.native == nil || !work.capture.valid(work.native.mutation) || work.scanner == nil || *work.scanner != work.scannerShape || work.native.scalar != work.scalarSource || work.native.typedNumber != work.numberSource || work.native.number != work.floatSource || work.native.typeError != work.errorSource ||
		!reflect.DeepEqual(legacyNativeObjectCheckpointControlFor(work.native, work.state.Model, work.state.Binding, work.state.SourceBytes), work.shape) {
		return ErrConflict
	}
	if work.state.AnyGraphNamespace != 0 && (work.anyGraph == nil || work.anyGraph.closed) {
		if work.anyGraph != nil && work.anyGraph.failure != nil {
			return work.anyGraph.failure
		}
		return ErrConflict
	}
	for index, previous := range work.guards {
		current, err := legacyNativeObjectCheckpointFieldValue(work.native, index)
		if err != nil {
			return err
		}
		if current != previous {
			return ErrConflict
		}
	}
	return nil
}

func (work *legacyNativeObjectCheckpoint) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	var err error
	switch work.phase {
	case 0:
		body, failure := work.scanner.checkpoint(ctx)
		if failure != nil {
			return work.fail(failure)
		}
		work.state.Scanner, err = legacyNativeCheckpointWrite(ctx, work.store, body)
		if err == nil {
			work.phase++
		}
	case 1:
		if work.field == len(work.guards) {
			work.phase++
			break
		}
		guard := work.guards[work.field]
		row := &work.state.Fields[work.field]
		field := work.native.metadata.fields[work.field]
		handled, failure := work.captureEventField(ctx)
		if failure != nil {
			err = failure
			break
		}
		if handled {
			break
		}
		if field.stringList {
			var done bool
			done, err = work.writeStringField(ctx)
			if err == nil && done {
				work.field++
				work.fieldPhase = 0
			}
			break
		}
		if work.native.metadata.model.Field(field.index).Type.Kind() == reflect.Int {
			*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: true, Integer: guard.integer}
			work.field++
			break
		}
		if work.text == nil {
			*row = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, Dynamic: guard.dynamic}
			if !row.Initialized || !row.Present {
				work.field++
				break
			}
			capacity := len(guard.text) + int(guard.excess/2)
			row.Value.State = legacyNativeScalarCheckpoint{Version: 1, Maximum: int(legacyNativeValueCheckpointPayloadLimit()), Bytes: len(guard.text), Capacity: capacity, Present: true}
			text := guard.text
			work.text, err = newLegacyNativeCheckpointTextWriter(ctx, len(text), func(current context.Context, offset, length int) ([]byte, error) {
				if err := current.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || offset > len(text) || length > len(text)-offset {
					return nil, ErrIntegrity
				}
				return []byte(text[offset : offset+length]), nil
			}, work.store)
			break
		}
		var ready bool
		ready, err = work.text.step(ctx)
		if err == nil && ready {
			row.Value.Text, err = work.text.take(ctx)
			work.text = nil
			if err == nil {
				work.field++
			}
		}
	case 2:
		if work.native.scalar == nil {
			work.phase++
			break
		}
		if work.scalar == nil {
			work.scalar, err = newLegacyNativeCheckpointScalarWriter(ctx, work.native.scalar, work.store)
			break
		}
		var ready bool
		ready, err = work.scalar.step(ctx)
		if err == nil && ready {
			work.state.Scalar, err = work.scalar.take(ctx)
			work.scalar = nil
			if err == nil {
				work.phase++
			}
		}
	case 3:
		if work.native.typedNumber == nil {
			work.phase++
			break
		}
		if work.child == nil {
			remaining, failure := work.remaining()
			if failure != nil {
				return work.fail(failure)
			}
			work.child, err = newLegacyNativeCheckpointTypedNumberWriter(ctx, work.native.typedNumber, work.source, work.store, remaining)
			break
		}
		var ready bool
		ready, err = work.child.step(ctx)
		if err == nil && ready {
			work.state.Number, err = work.child.take(ctx)
			work.child = nil
			if err == nil {
				work.phase++
			}
		}
	case 4:
		if work.native.typeError == nil {
			work.phase++
			break
		}
		if work.child == nil {
			remaining, failure := work.remaining()
			if failure != nil {
				return work.fail(failure)
			}
			work.child, err = newLegacyNativeCheckpointTypeErrorWriter(ctx, work.native.typeError, work.store, work.source.Bytes, remaining)
			break
		}
		var ready bool
		ready, err = work.child.step(ctx)
		if err == nil && ready {
			work.state.Error, err = work.child.take(ctx)
			work.child = nil
			if err == nil {
				work.phase++
			}
		}
	case 5:
		if work.native.number == nil {
			work.phase++
			break
		}
		if work.floatTask == nil {
			remaining, failure := work.remaining()
			if failure != nil {
				return work.fail(failure)
			}
			work.floatTask, err = newLegacyNativeObjectFloatCheckpoint(ctx, work.native, work.state.Binding, work.source, work.store, remaining)
			break
		}
		var ready bool
		ready, err = work.floatTask.step(ctx)
		if err == nil && ready {
			var planned docRangeHeaderHeapEstimate
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointRef{})))
			if err = work.admit(planned.bytes); err != nil {
				return work.fail(err)
			}
			var reference legacyNativeCheckpointRef
			reference, err = work.floatTask.take(ctx)
			work.floatTask = nil
			if err == nil {
				work.state.FloatNumber = &reference
				work.phase++
			}
		}
	case 6:
		if work.state.InitialGraphCount == 0 {
			work.phase++
			break
		}
		if work.initialTask == nil {
			work.initialTask, err = newLegacyNativeCheckpointGoGraphSetForObject(ctx, work)
			break
		}
		var ready bool
		ready, err = work.initialTask.step(ctx)
		if err == nil && ready {
			var planned docRangeHeaderHeapEstimate
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointRef{})))
			if err = work.admit(planned.bytes); err != nil {
				return work.fail(err)
			}
			var reference legacyNativeCheckpointRef
			reference, err = work.initialTask.take(ctx)
			work.initialTask = nil
			if err == nil {
				work.state.InitialGraphs = &reference
				work.phase++
			}
		}
	case 7:
		if err = work.state.validate(work.state.Model, work.state.Binding, work.source.Bytes, true); err == nil {
			work.root, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
			if err == nil {
				work.ready = true
			}
		}
	default:
		err = ErrIntegrity
	}
	if err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func legacyNativeObjectCheckpointControlHeap(state legacyNativeObjectCheckpointControl) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(len(state.Binding)+len(state.Schema)+len(state.Scanner.Hash)+len(state.Number.Hash)+len(state.Error.Hash)), 1)
	heap.allocation(int64(cap(state.Key)), 1)
	heap.allocation(int64(cap(state.Fields)), int64(unsafe.Sizeof(legacyNativeObjectCheckpointField{})))
	heap.allocation(int64(len(state.Scalar.Text.Tree.Ref.Hash)), 1)
	if state.FloatNumber != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*state.FloatNumber)))
		heap.allocation(int64(len(state.FloatNumber.Hash)), 1)
	}
	if state.InitialGraphs != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*state.InitialGraphs)))
		heap.allocation(int64(len(state.InitialGraphs.Hash)), 1)
	}
	for _, row := range state.Fields {
		if row.Event != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*row.Event)))
			heap.allocation(int64(len(row.Event.List.Hash)), 1)
		}
		if row.Any != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*row.Any)))
			heap.allocation(int64(len(row.Any.Parser.Hash)), 1)
			if row.Any.Destination != nil {
				heap.allocation(1, int64(unsafe.Sizeof(*row.Any.Destination)))
				legacyNativeCheckpointGraphCapturedValueHeap(&heap, *row.Any.Destination)
			}
		}
		heap.allocation(int64(len(row.Value.Text.Tree.Ref.Hash)), 1)
		heap.allocation(int64(len(row.Strings.Items.Ref.Hash)+len(row.StringParser.Hash)), 1)
		if row.Block != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*row.Block)))
			heap.allocation(int64(len(row.Block.Destination.Records.Ref.Hash)+len(row.Block.Parser.Hash)), 1)
		}
		if row.Nested != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*row.Nested)))
			heap.allocation(int64(len(row.Nested.Parser.Hash)), 1)
			if row.Nested.Value != nil {
				heap.allocation(1, int64(unsafe.Sizeof(*row.Nested.Value)))
				heap.allocation(int64(len(row.Nested.Value.Schema)+len(row.Nested.Value.Values.Items.Ref.Hash)), 1)
			}
		}
	}
	return heap
}

func (work *legacyNativeObjectCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	shared, err := work.anyGraph.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: shared, err: err})
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	if work.capture != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.capture)))
	}
	heap.include(legacyNativeObjectCheckpointControlHeap(work.state))
	heap.include(legacyNativeObjectCheckpointControlHeap(work.shape))
	heap.allocation(int64(cap(work.initialSlots)), 1)
	heap.allocation(int64(cap(work.initialValues)), int64(unsafe.Sizeof(any(nil))))
	heap.allocation(int64(cap(work.guards)), int64(unsafe.Sizeof(legacyNativeObjectCheckpointValueGuard{})))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.root.Hash)), 1)
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.text, work.scalar, work.child, work.strings, work.blockTask, work.nestedTask, work.anyTask, work.floatTask, work.initialTask, work.resourceTask} {
		if current != nil {
			retained, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	if work.eventTask != nil {
		retained, err := work.eventTask.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	source := work.eventSource
	if !work.state.HasEventCache && work.eventOwner == nil {
		source, err = work.native.retained()
		if err != nil {
			return 0, err
		}
	}
	if retained < 0 || source < 0 || retained > work.budget || source > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - source, nil
}
func (work *legacyNativeObjectCheckpoint) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}
func (work *legacyNativeObjectCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}
func (work *legacyNativeObjectCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if err := work.admit(0); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	root := work.root
	work.close()
	return root, nil
}
func (work *legacyNativeObjectCheckpoint) close() {
	if work != nil {
		for _, current := range []interface{ close() }{work.text, work.scalar, work.child, work.strings, work.blockTask, work.nestedTask, work.anyTask, work.floatTask, work.initialTask, work.resourceTask} {
			if current != nil {
				current.close()
			}
		}
		work.anyGraph.close()
		work.eventTask.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNativeObjectCheckpoint{closed: true, failure: failure}
	}
}

type legacyNativeObjectCheckpointNumberRecovery interface {
	legacyNativeAnyCheckpointTask[*legacyNativeTypedNumber]
	source() LegacyByteSource
}

type legacyNativeObjectCheckpointOwner struct {
	decoder *LegacyNormalizationDecoder
	event   *legacyNormalizationNativeEvent
	list    *legacyNativeEventListCheckpointRestore
	header  *legacyNormalizationHeader
}

func (owner legacyNativeObjectCheckpointOwner) validate(granted bool) error {
	if owner.list != nil {
		if owner.decoder != nil || owner.event != nil || owner.header != nil || granted {
			return ErrIntegrity
		}
		return owner.list.checkOwner()
	}
	if owner.decoder == nil {
		if owner.event != nil || owner.header != nil || granted {
			return ErrIntegrity
		}
		return nil
	}
	scope := owner.decoder.mutation
	if scope == nil {
		return ErrIntegrity
	}
	if scope.closed.Load() {
		return ErrConflict
	}
	expected := int64(0)
	if granted || owner.header != nil {
		expected = -2
	}
	if scope.activity.Load() != expected || scope.stepping.Load() {
		return ErrLegacyNormalizationBusy
	}
	return nil
}

func (owner legacyNativeObjectCheckpointOwner) retainedBytes() (int64, error) {
	if owner.decoder == nil {
		return 0, nil
	}
	retained, err := owner.decoder.RetainedBytes()
	var heap docRangeHeaderHeapEstimate
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if owner.event != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*owner.event)))
	}
	return heap.bytes, heap.err
}

type legacyNativeObjectCheckpointRestore struct {
	state           legacyNativeObjectCheckpointControl
	ref             legacyNativeCheckpointRef
	model           uint8
	binding         ContentHash
	native          *legacyNativeObject
	scanner         legacyJSONValueScanner
	source          LegacyByteSource
	store           legacyNativeCheckpointIO
	owner           legacyNativeObjectCheckpointOwner
	ownerGranted    bool
	scalar          legacyNativeAnyCheckpointTask[*legacyNativeScalar]
	number          legacyNativeObjectCheckpointNumberRecovery
	semantic        legacyNativeAnyCheckpointTask[*json.UnmarshalTypeError]
	strings         legacyNativeAnyCheckpointTask[legacyNativeCheckpointStringValues]
	list            legacyNativeAnyCheckpointTask[*legacyNativeStringList]
	blockTask       legacyNativeAnyCheckpointTask[legacyNativeObjectBlockRestored]
	eventTask       *legacyNativeEventListCheckpointRestore
	eventFrame      *legacyJSONValueScanner
	nestedTask      legacyNativeAnyCheckpointTask[legacyNativeObjectNestedRestored]
	anyTask         legacyNativeAnyCheckpointTask[legacyNativeObjectAnyRestored]
	anyGraph        *legacyNativeCheckpointGraphRestored
	sharedNative    int64
	floatTask       legacyNativeAnyCheckpointTask[legacyNativeObjectFloatRestored]
	initialTask     legacyNativeAnyCheckpointTask[[]legacyNativeAnyValue]
	aliasTask       *legacyNativeInitialAliasPreparation
	initialValues   []legacyNativeAnyValue
	fieldPhase      uint8
	field           int
	stringHeapIndex int
	stringHeap      docRangeHeaderHeapEstimate
	phase           uint8
	budget          int64
	ready           bool
	closed          bool
	failure         error
}

func newLegacyNativeObjectCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, model uint8, store legacyNativeCheckpointIO, source LegacyByteSource, binding ContentHash, budget int64) (*legacyNativeObjectCheckpointRestore, error) {
	return newLegacyNativeObjectCheckpointRestoreForOwner(ctx, ref, model, store, source, binding, budget, legacyNativeObjectCheckpointOwner{})
}

func newLegacyNativeObjectCheckpointRestoreForOwner(ctx context.Context, ref legacyNativeCheckpointRef, model uint8, store legacyNativeCheckpointIO, source LegacyByteSource, binding ContentHash, budget int64, owner legacyNativeObjectCheckpointOwner) (*legacyNativeObjectCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.validate(false); err != nil {
		return nil, err
	}
	granted := false
	if owner.decoder != nil && owner.header == nil {
		if !owner.decoder.mutation.activity.CompareAndSwap(0, -2) {
			return nil, ErrLegacyNormalizationBusy
		}
		granted = true
		defer func() {
			if granted {
				owner.decoder.mutation.activity.CompareAndSwap(-2, 0)
			}
		}()
		if err := owner.validate(true); err != nil {
			return nil, err
		}
	}
	if owner.list != nil && model != legacyNativeCheckpointEventModel || owner.decoder != nil && (model != legacyNativeCheckpointEnvelopeModel && model != legacyNativeCheckpointEventModel || (model == legacyNativeCheckpointEventModel) != (owner.event != nil) || owner.event != nil && (owner.event.object != nil || owner.event.canonicalReady) || owner.decoder.native != nil || owner.decoder.nativeCache != nil || owner.header == nil && owner.decoder.finalizing != nil || owner.decoder.terminal != nil) {
		return nil, ErrIntegrity
	}
	if owner.header != nil && (model != legacyNativeCheckpointEnvelopeModel || owner.event != nil || owner.decoder.phase != legacyNormalizationFinalizing || owner.decoder.finalizing != owner.header || owner.header.native != nil || owner.header.encoding == nil || owner.header.encoding.initialized || !reflect.ValueOf(owner.header.encoding.envelope).IsZero()) {
		return nil, ErrIntegrity
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || len(binding) != len(contentHashPrefix)+64 || !ref.valid() || ValidateContentHash(binding) != nil || legacyNativeCheckpointObjectModel(model) == nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return nil, err
	}
	if owner.decoder != nil && (owner.decoder.source.authority == 0 || owner.decoder.source.authority != source.authority || owner.decoder.source.Bytes != source.Bytes || owner.decoder.source.partBytes() != source.partBytes() || len(owner.decoder.source.Parts) != len(source.Parts)) {
		return nil, ErrIntegrity
	}
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(source.Parts[index].Hash) != len(contentHashPrefix)+64 {
			return nil, ErrIntegrity
		}
		if err := legacyNativeAnyCheckpointSourcePart(source, index); err != nil {
			return nil, err
		}
		if owner.decoder != nil && owner.decoder.source.Parts[index] != source.Parts[index] {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	ownerBytes, err := owner.retainedBytes()
	planned.include(docRangeHeaderHeapEstimate{bytes: ownerBytes, err: err})
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	parts := make([]ConversationManifestChunk, len(source.Parts))
	copy(parts, source.Parts)
	source.Parts = parts
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source.Parts[index].Hash = ContentHash(strings.Clone(string(source.Parts[index].Hash)))
	}
	work := &legacyNativeObjectCheckpointRestore{ref: ref, model: model, binding: ContentHash(strings.Clone(string(binding))), store: store, source: source, budget: budget, owner: owner, ownerGranted: granted}
	granted = false
	return work, nil
}

func (work *legacyNativeObjectCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.model, work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		metadata := legacyNativeCheckpointObjectModel(work.model)
		var planned docRangeHeaderHeapEstimate
		if work.owner.decoder == nil && work.owner.list == nil {
			planned.allocation(1, int64(metadata.model.Size()))
		}
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObject{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
		planned.allocation(int64(work.state.RawCapacity+metadata.keyBytes), 1)
		planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})+unsafe.Sizeof(int64(0))))
		if work.state.HasStringCache {
			planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof((*legacyNativeStringList)(nil))))
		}
		if work.state.HasBlockCache {
			planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof((*legacyNativeBlockList)(nil))))
		}
		if work.state.HasEventCache {
			planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof((*legacyNativeEventList)(nil))))
			planned.allocation(1, int64(unsafe.Sizeof(legacyJSONValueScanner{})))
		}
		if work.state.HasNestedCache {
			planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof((*legacyNativeObject)(nil))))
		}
		if work.state.HasContainerCache {
			planned.allocation(int64(len(metadata.fields)), int64(unsafe.Sizeof((*legacyNativeAny)(nil))))
		}
		if work.state.AnyGraphNamespace != 0 {
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestored{})))
		}
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		if err := work.owner.validate(work.ownerGranted); err != nil {
			return err
		}
		var destination reflect.Value
		if work.owner.list != nil {
			destination = reflect.ValueOf(&work.owner.list.current.list.child)
		} else if work.owner.decoder == nil {
			destination = reflect.New(metadata.model)
		} else {
			if work.owner.header != nil {
				destination = reflect.ValueOf(&work.owner.header.encoding.envelope)
			} else if work.owner.event != nil {
				destination = reflect.ValueOf(&work.owner.event.decoded)
			} else {
				if work.owner.decoder.envelopeNative != nil {
					return ErrIntegrity
				}
				destination = reflect.ValueOf(&work.owner.decoder.envelope)
			}
			if !destination.IsValid() || destination.Type() != reflect.PointerTo(metadata.model) || destination.IsNil() || !destination.Elem().CanSet() || !destination.Elem().IsZero() {
				return ErrIntegrity
			}
		}
		native, err := newLegacyNativeObject(destination, metadata, work.state.ParentDepth, work.state.TrackPresence)
		if err != nil {
			return err
		}
		if work.owner.decoder != nil {
			native.mutation = work.owner.decoder.mutation
		} else if work.owner.list != nil {
			native.mutation = work.owner.list.scope
		}
		state := work.state
		native.parentDepth, native.trackPresence, native.presence = state.ParentDepth, state.TrackPresence, state.Presence
		native.pagedEvents, native.deferTypeErrors, native.skipValue = state.PagedEvents, state.DeferTypeErrors, state.SkipValue
		native.fieldLimit, native.prefix = state.FieldLimit, state.Prefix
		native.position, native.end, native.valueStart, native.valueEnd, native.field = state.Position, state.End, state.ValueStart, state.ValueEnd, state.Field
		native.phase, native.valueFirst, native.keyStarted, native.keyOverflow, native.valueStarted = state.Phase, state.ValueFirst, state.KeyStarted, state.KeyOverflow, state.ValueStarted
		native.listActive = state.ListActive
		native.blockActive = state.BlockActive
		native.eventActive = state.EventActive
		native.nestedActive = state.NestedActive
		native.containerActive = state.ContainerActive
		if state.Model == legacyNativeCheckpointEventModel {
			native.destination.Interface().(*wire).fieldPresence = state.DestinationPresence
		}
		if state.HasBlockCache {
			native.blocks = make([]*legacyNativeBlockList, len(metadata.fields))
		}
		if state.HasEventCache {
			native.events = make([]*legacyNativeEventList, len(metadata.fields))
		}
		if state.HasStringCache {
			native.lists = make([]*legacyNativeStringList, len(metadata.fields))
		}
		if state.HasNestedCache {
			native.nested = make([]*legacyNativeObject, len(metadata.fields))
		}
		if state.HasContainerCache {
			native.containers = make([]*legacyNativeAny, len(metadata.fields))
		}
		native.key = append(native.key, state.Key...)
		if state.RawCapacity > 0 {
			native.raw = make([]byte, 0, state.RawCapacity)
		}
		work.native = native
		if state.AnyGraphNamespace != 0 {
			work.anyGraph = newLegacyNativeCheckpointGraphRestored()
		}
		work.phase++
	case 1:
		body, err := legacyNativeCheckpointRead(ctx, work.store, work.state.Scanner)
		if err != nil {
			return err
		}
		work.scanner, err = restoreLegacyJSONValueScanner(ctx, body)
		if err != nil {
			return err
		}
		if work.owner.list != nil {
			work.eventFrame = work.owner.list.scanner
		} else if work.state.HasEventCache {
			if work.owner.decoder != nil {
				work.eventFrame = &work.owner.decoder.frame
			} else {
				work.eventFrame = new(legacyJSONValueScanner)
				*work.eventFrame = work.scanner
				work.native.checkpointScanner = work.eventFrame
			}
		}
		if work.eventFrame != nil && *work.eventFrame != work.scanner {
			return ErrIntegrity
		}
		work.phase++
	case 2:
		if work.state.InitialGraphCount != 0 && work.initialValues == nil {
			return work.restoreInitialGraphs(ctx)
		}
		return work.restoreField(ctx)
	case 3:
		if !work.state.HasScalar {
			work.phase++
			break
		}
		if work.scalar == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointScalarRestore(ctx, work.state.Scalar, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.scalar = current
			break
		}
		ready, err := work.scalar.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.native.scalar, err = work.scalar.take(ctx)
		work.scalar = nil
		if err != nil {
			return err
		}
		work.phase++
	case 4:
		if work.state.Number == (legacyNativeCheckpointRef{}) {
			work.phase++
			break
		}
		if work.number == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			field := work.native.metadata.fields[work.state.Field]
			prior, _ := work.native.destination.Elem().Field(field.index).Interface().(*json.Number)
			current, err := newLegacyNativeCheckpointTypedNumberRestore(ctx, work.state.Number, work.store, work.source, prior, remaining)
			if err != nil {
				return err
			}
			work.number = current
			break
		}
		ready, err := work.number.step(ctx)
		if err != nil || !ready {
			return err
		}
		source := work.number.source()
		if err := legacyNativeAnyCheckpointSourceGeometry(source, work.source.Bytes); err != nil {
			return err
		}
		if source.authority != work.source.authority {
			return ErrIntegrity
		}
		work.native.typedNumber, err = work.number.take(ctx)
		work.number = nil
		if err != nil {
			return err
		}
		work.source = source
		number := work.native.typedNumber
		if number.field != work.state.Field || number.from != work.state.ValueStart || number.end != work.state.ValueEnd || number.quoted != (work.state.ValueFirst == '"') {
			return ErrIntegrity
		}
		work.phase++
	case 5:
		if work.state.Error == (legacyNativeCheckpointRef{}) {
			work.phase++
			break
		}
		if work.semantic == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointTypeErrorRestore(ctx, work.state.Error, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.semantic = current
			break
		}
		ready, err := work.semantic.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.native.typeError, err = work.semantic.take(ctx)
		work.semantic = nil
		if err != nil {
			return err
		}
		work.phase++
	case 6:
		if !work.state.HasFloatNumber {
			work.phase++
			break
		}
		if work.floatTask == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeObjectFloatCheckpointRestore(ctx, *work.state.FloatNumber, work.binding, work.source, work.store, remaining)
			if err != nil {
				return err
			}
			work.floatTask = current
			break
		}
		ready, err := work.floatTask.step(ctx)
		if err != nil || !ready {
			return err
		}
		result, err := work.floatTask.take(ctx)
		work.floatTask = nil
		if err != nil {
			return err
		}
		if result.Number == nil || result.Number.parser == nil || result.Number.parser.from != work.state.ValueStart || result.Number.parser.end != work.state.ValueEnd || result.Source.authority != work.source.authority {
			return ErrIntegrity
		}
		var adopted docRangeHeaderHeapEstimate
		result.Number.retained(&adopted)
		adopted.include(legacyNativeAnyCheckpointSourceHeap(result.Source))
		if adopted.err != nil {
			return adopted.err
		}
		if err := work.admit(adopted.bytes); err != nil {
			return err
		}
		work.native.number, work.source = result.Number, result.Source
		work.phase++
	case 7:
		ready, err := work.prepareInitialAlias(ctx)
		if err != nil || !ready {
			return err
		}
		if err := work.validateComplete(); err != nil {
			return err
		}
		if err := work.checkSharedHandoff(ctx); err != nil {
			return err
		}
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeObjectCheckpointRestore) restoreField(ctx context.Context) error {
	if work.field == len(work.state.Fields) {
		work.phase++
		return nil
	}
	row := work.state.Fields[work.field]
	metadata := work.native.metadata.fields[work.field]
	value := work.native.destination.Elem().Field(metadata.index)
	if handled, err := work.restoreEventField(ctx); handled || err != nil {
		return err
	}
	if metadata.stringList {
		return work.restoreStringField(ctx)
	}
	if value.Kind() == reflect.Int {
		value.SetInt(row.Integer)
		if row.Initialized {
			work.native.heaps[work.field].allocation(1, int64(value.Type().Size()))
		}
		work.field++
		return nil
	}
	if !row.Initialized {
		work.field++
		return nil
	}
	if !row.Present {
		heap, err := legacyNativeValueHeap(ctx, value)
		if err != nil {
			return err
		}
		work.native.heaps[work.field] = heap
		work.field++
		return nil
	}
	if work.scalar == nil {
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		current, err := newLegacyNativeCheckpointScalarRestore(ctx, row.Value, work.store, work.source.Bytes, remaining)
		if err != nil {
			return err
		}
		work.scalar = current
		return nil
	}
	ready, err := work.scalar.step(ctx)
	if err != nil || !ready {
		return err
	}
	scalar, err := work.scalar.take(ctx)
	if err != nil {
		return err
	}
	work.scalar = nil
	if scalar.text == nil || scalar.text.Len() != row.Value.State.Bytes || scalar.text.Cap() != row.Value.State.Capacity && row.Value.State.Capacity != row.Value.State.Bytes {
		return ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(value.Type().Size()))
	if value.Kind() == reflect.Pointer {
		heap.allocation(1, int64(unsafe.Sizeof(json.Number(""))))
	}
	if value.Kind() == reflect.Interface {
		heap.allocation(1, int64(unsafe.Sizeof(string(""))))
	}
	heap.allocation(int64(scalar.text.Cap()), 1)
	if heap.err != nil {
		return heap.err
	}
	if err := work.admit(heap.bytes); err != nil {
		return err
	}
	if value.Kind() == reflect.String || value.Kind() == reflect.Interface {
		if value.Kind() == reflect.String {
			value.SetString(scalar.text.String())
		} else {
			value.Set(reflect.ValueOf(scalar.text.String()))
		}
	} else {
		number := json.Number(scalar.text.String())
		value.Set(reflect.ValueOf(&number))
	}
	work.native.heaps[work.field] = heap
	work.native.backingExcess[work.field] = 2 * int64(scalar.text.Cap()-scalar.text.Len())
	work.field++
	return nil
}

func (work *legacyNativeObjectCheckpointRestore) validateComplete() error {
	if work.native == nil || work.field != len(work.native.metadata.fields) || work.scalar != nil || work.number != nil || work.semantic != nil || work.strings != nil || work.list != nil || work.blockTask != nil || work.eventTask != nil || work.nestedTask != nil || work.anyTask != nil || work.floatTask != nil || work.initialTask != nil || len(work.initialValues) != int(work.state.InitialGraphCount) || (work.native.number != nil) != work.state.HasFloatNumber {
		return ErrIntegrity
	}
	if err := legacyNativeObjectCheckpointScanner(work.state, &work.scanner); err != nil {
		return err
	}
	for index := range work.native.metadata.fields {
		if _, err := legacyNativeObjectCheckpointFieldValue(work.native, index); err != nil {
			return err
		}
	}
	if number := work.native.typedNumber; number != nil {
		field := work.native.destination.Elem().Field(work.native.metadata.fields[work.state.Field].index)
		prior, _ := field.Interface().(*json.Number)
		if number.newTarget && prior != nil || !number.newTarget && prior != number.target || number.authority != work.source.authority {
			return ErrIntegrity
		}
	}
	return nil
}

func (work *legacyNativeObjectCheckpointRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := work.owner.validate(work.ownerGranted); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeObjectCheckpointControlHeap(work.state))
	ownerBytes, ownerErr := work.owner.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: ownerBytes, err: ownerErr})
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)), 1)
	heap.include(legacyNativeObjectInitialValuesHeap(work.initialValues))
	shared, err := work.anyGraph.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: shared, err: err})
	if work.native != nil {
		var retained int64
		var err error
		if work.owner.list != nil {
			retained, err = work.native.retainedBase()
		} else {
			retained, err = work.native.retained()
		}
		if retained < work.sharedNative || work.sharedNative < 0 {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - work.sharedNative, err: err})
		if work.owner.decoder == nil && work.owner.list == nil {
			heap.allocation(1, int64(work.native.metadata.model.Size()))
		}
	}
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.scalar, work.number, work.semantic, work.strings, work.list, work.blockTask, work.eventTask, work.nestedTask, work.anyTask, work.floatTask, work.initialTask, work.aliasTask} {
		if current != nil {
			retained, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	return heap.bytes, heap.err
}
func (work *legacyNativeObjectCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}
func (work *legacyNativeObjectCheckpointRestore) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}
func (work *legacyNativeObjectCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}
func (work *legacyNativeObjectCheckpointRestore) take(ctx context.Context) (legacyNativeObjectRestored, error) {
	if work == nil {
		return legacyNativeObjectRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectRestored{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectRestored{}, ErrIntegrity
	}
	if err := work.owner.validate(work.ownerGranted); err != nil {
		work.fail(err)
		return legacyNativeObjectRestored{}, err
	}
	if err := work.validateComplete(); err != nil {
		work.fail(err)
		return legacyNativeObjectRestored{}, err
	}
	if err := work.checkSharedHandoff(ctx); err != nil {
		work.fail(err)
		return legacyNativeObjectRestored{}, err
	}
	result := legacyNativeObjectRestored{Object: work.native, Scanner: work.scanner, Source: work.source}
	if err := work.retainCheckpointScanner(ctx); err != nil {
		work.fail(err)
		return legacyNativeObjectRestored{}, err
	}
	work.close()
	return result, nil
}
func (work *legacyNativeObjectCheckpointRestore) close() {
	if work != nil {
		for _, current := range []interface{ close() }{work.scalar, work.number, work.semantic, work.strings, work.list, work.blockTask, work.eventTask, work.nestedTask, work.anyTask, work.floatTask, work.initialTask, work.aliasTask} {
			if current != nil {
				current.close()
			}
		}
		work.anyGraph.close()
		if work.ownerGranted {
			work.owner.decoder.mutation.activity.CompareAndSwap(-2, 0)
		}
		failure := work.failure
		*work = legacyNativeObjectCheckpointRestore{closed: true, failure: failure}
	}
}
