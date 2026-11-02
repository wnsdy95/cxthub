package domain

import (
	"reflect"
	"unsafe"
)

type legacyNativeEventListCheckpointControl struct {
	Version     uint8                     `json:"version"`
	Binding     ContentHash               `json:"binding"`
	SourceBytes int                       `json:"source_bytes"`
	Index       int                       `json:"index"`
	Parent      int                       `json:"parent"`
	Cursor      int                       `json:"cursor"`
	Position    int                       `json:"position"`
	End         int                       `json:"end"`
	ParentDepth int                       `json:"parent_depth"`
	Maximum     int                       `json:"maximum"`
	Count       int                       `json:"count"`
	Copied      int                       `json:"copied"`
	ChildStart  int                       `json:"child_start"`
	Literal     int                       `json:"literal"`
	Phase       uint8                     `json:"phase"`
	First       byte                      `json:"first"`
	Prefix      bool                      `json:"prefix"`
	DecodedNil  bool                      `json:"decoded_nil"`
	HasValues   bool                      `json:"has_values"`
	HasFrame    bool                      `json:"has_frame"`
	Borrowed    int64                     `json:"borrowed"`
	Child       uint8                     `json:"child"`
	Data        legacyNativeCheckpointRef `json:"data"`
	Frame       legacyNativeCheckpointRef `json:"frame"`
	Object      legacyNativeCheckpointRef `json:"object"`
}

func legacyNativeEventListCheckpointControlFor(list *legacyNativeEventList, binding ContentHash, sourceBytes, index, parentIndex, cursorIndex int) (legacyNativeEventListCheckpointControl, error) {
	if list == nil {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if !list.paged {
		return legacyNativeEventListCheckpointControl{}, ErrDocRangeIndexPending
	}
	if cap(list.decoded) != 0 || list.previous != nil || list.owned || list.previousOwned || list.copied != 0 ||
		list.records != (legacyCanonicalPages[legacyNativeEventRecord]{}) || list.payload != (docRangeHeaderHeapEstimate{}) ||
		list.added != (docRangeHeaderHeapEstimate{}) || list.borrowed.err != nil || list.excess < 0 ||
		(list.parent == nil) != (parentIndex == -1) || (list.cursor == nil) != (cursorIndex == -1) ||
		list.parent == list || list.cursor != nil && ((list.cursor == list) != (cursorIndex == index) || list.cursor == list.parent) {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	state := legacyNativeEventListCheckpointControl{
		Version: legacyNativeCheckpointVersion, Binding: binding, SourceBytes: sourceBytes,
		Index: index, Parent: parentIndex, Cursor: cursorIndex, Position: list.position, End: list.end,
		ParentDepth: list.parentDepth, Maximum: list.maximum, Count: list.count, Copied: list.copied,
		ChildStart: list.childStart, Literal: list.literal, Phase: list.phase, First: list.first, Prefix: list.prefix,
		DecodedNil: list.decoded == nil, HasValues: list.values != nil, HasFrame: list.frame != nil, Borrowed: list.borrowed.bytes,
	}
	if list.object != nil {
		state.Child = 1
		if list.phase == legacyNativeEventsApply {
			state.Child = 2
		}
	}
	if err := state.validate(binding, sourceBytes, false); err != nil {
		return legacyNativeEventListCheckpointControl{}, err
	}
	if list.values != nil && (list.values.count != list.count || list.values.records.length() > sourceBytes) {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	heap.include(list.values.heap())
	if heap.err != nil || list.estimate != heap {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if state.Child != 2 && !reflect.ValueOf(list.child).IsZero() {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	object := list.object
	if object == nil {
		return state, nil
	}
	if object.metadata != &legacyEventNativeFields || legacyEventNativeFields.err != nil ||
		object.destination.Kind() != reflect.Pointer || object.destination.IsNil() || object.destination.Type() != reflect.TypeFor[*wire]() ||
		object.destination.Pointer() != uintptr(unsafe.Pointer(&list.child)) || object.mutation != list.mutation ||
		object.parentDepth != list.parentDepth+1 || !object.trackPresence || !object.pagedEvents ||
		len(object.heaps) != len(legacyEventNativeFields.fields) || len(object.backingExcess) != len(legacyEventNativeFields.fields) ||
		cap(object.key) != legacyEventNativeFields.keyBytes {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if state.Child == 1 {
		if !legacyNativeEventListCheckpointResetChild(object) {
			return legacyNativeEventListCheckpointControl{}, ErrIntegrity
		}
		return state, nil
	}
	if object.phase > legacyNativeObjectDone || object.position < list.childStart || object.position > object.end ||
		object.end <= list.childStart || object.end > list.end {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	if object.prefix {
		if list.first != '{' || list.parentDepth >= 10000 || object.end != list.end || list.position != list.childStart {
			return legacyNativeEventListCheckpointControl{}, ErrIntegrity
		}
	} else if list.first != 'n' || list.position-list.childStart != len("null") || object.end != list.position ||
		object.field != -1 || object.phase != legacyNativeObjectStart && object.phase != legacyNativeObjectDone ||
		!reflect.ValueOf(list.child).IsZero() {
		return legacyNativeEventListCheckpointControl{}, ErrIntegrity
	}
	return state, nil
}

func legacyNativeEventListCheckpointResetChild(object *legacyNativeObject) bool {
	if object.phase != legacyNativeObjectStart || object.position != 0 || object.end != 0 || object.field != -1 ||
		len(object.key) != 0 || len(object.raw) != 0 || object.checkpointScanner != nil ||
		object.scalar != nil || object.number != nil || object.typedNumber != nil ||
		object.lists != nil || object.blocks != nil || object.events != nil || object.nested != nil || object.containers != nil ||
		object.initialAlias != (legacyNativeInitialAlias{}) || object.listActive || object.blockActive || object.eventActive ||
		object.nestedActive || object.containerActive || object.deferTypeErrors || object.typeError != nil || object.skipValue ||
		object.fieldLimit != 0 || object.prefix || object.presence != 0 || object.valueFirst != 0 ||
		object.keyStarted || object.keyOverflow || object.valueStarted {
		return false
	}
	for index, heap := range object.heaps {
		if heap != (docRangeHeaderHeapEstimate{}) || object.backingExcess[index] != 0 {
			return false
		}
	}
	return true
}

func (state legacyNativeEventListCheckpointControl) validate(binding ContentHash, sourceBytes int, ready bool) error {
	if state.Version != legacyNativeCheckpointVersion || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || state.Binding != binding ||
		sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || state.SourceBytes != sourceBytes ||
		state.Index < 0 || state.Index >= 10000 || state.Parent < -1 || state.Parent != -1 && state.Parent != state.Index-1 ||
		state.Cursor < -1 || state.Cursor != -1 && (state.Cursor < state.Index || state.Cursor >= 10000) ||
		state.Position < 0 || state.End <= 0 || state.Position > state.End || state.End > sourceBytes ||
		state.ParentDepth < 1 || state.ParentDepth > 10000 || state.Maximum < 0 || state.Maximum > state.End/2 ||
		state.Count < 0 || state.Count > state.Maximum || state.Copied != 0 || state.ChildStart < 0 || state.ChildStart > state.Position ||
		state.Literal < 0 || state.Literal > len("null") || state.Phase > legacyNativeEventsDone || state.Borrowed < 0 ||
		state.Child > 2 || (state.Child == 2) != (state.Phase == legacyNativeEventsApply) || !state.HasValues && state.Count != 0 {
		return ErrIntegrity
	}
	start := max(0, state.End-2*state.Maximum-1)
	if state.Position < start {
		return ErrIntegrity
	}
	switch state.Phase {
	case legacyNativeEventsStart:
		if state.Count != 0 || state.ChildStart != 0 || state.Literal != 0 || state.Position == state.End || state.Maximum != (state.End-state.Position)/2 {
			return ErrIntegrity
		}
	case legacyNativeEventsFirst:
		if state.Count != 0 || state.ChildStart != 0 || state.Literal != 0 || !state.HasValues || state.Position <= start {
			return ErrIntegrity
		}
	case legacyNativeEventsValue, legacyNativeEventsAfter:
		if state.Count == 0 || state.Child != 1 || state.ChildStart <= start || state.ChildStart >= state.Position || state.Literal != 0 || !state.HasValues {
			return ErrIntegrity
		}
	case legacyNativeEventsScan, legacyNativeEventsApply:
		if state.Count >= state.Maximum || state.ChildStart <= start || state.ChildStart >= state.End || state.Literal != 0 || !state.HasValues || !state.HasFrame ||
			state.Phase == legacyNativeEventsApply && state.First != '{' && state.First != 'n' {
			return ErrIntegrity
		}
	case legacyNativeEventsNull:
		if state.Count != 0 || state.ChildStart != 0 || state.Literal == len("null") || state.Position < state.Literal ||
			state.Maximum != (state.End-state.Position+state.Literal)/2 {
			return ErrIntegrity
		}
	case legacyNativeEventsTail, legacyNativeEventsDone:
		if state.Phase == legacyNativeEventsTail && state.Prefix || state.Phase == legacyNativeEventsDone && !state.Prefix && state.Position != state.End ||
			state.Literal != 0 && state.Literal != len("null") || state.HasValues != (state.Literal == 0) ||
			state.Literal == len("null") && (!state.DecodedNil || state.Count != 0) ||
			state.Count == 0 && state.ChildStart != 0 || state.Count > 0 && (state.Child != 1 || state.ChildStart <= start || state.ChildStart >= state.Position) {
			return ErrIntegrity
		}
	}
	if !ready {
		if state.Data != (legacyNativeCheckpointRef{}) || state.Frame != (legacyNativeCheckpointRef{}) || state.Object != (legacyNativeCheckpointRef{}) {
			return ErrIntegrity
		}
		return nil
	}
	if !state.Data.valid() || state.HasFrame && !state.Frame.valid() || !state.HasFrame && state.Frame != (legacyNativeCheckpointRef{}) ||
		state.Child == 2 && !state.Object.valid() || state.Child != 2 && state.Object != (legacyNativeCheckpointRef{}) {
		return ErrIntegrity
	}
	return nil
}
