package domain

import (
	"context"
	"encoding/json"
	"unsafe"
)

const legacyNativeEventRecordFieldRoots = 18

var legacyNativeEventRecordFieldNames = [...]string{
	"kind", "id", "ts", "seq", "provider_metadata", "role", "blocks", "compact_summary",
	"agent_message", "agent_author", "agent_recipient", "replacement", "replacement_complete",
	"call_id", "tool_name", "provider_tool_name", "input", "status", "output", "is_error",
	"locked", "redacted_summary", "cross_replayable",
}

func legacyNativeEventRecordFieldSchema() (ContentHash, error) {
	if len(legacyEventNativeFields.fields) != len(legacyNativeEventRecordFieldNames) {
		return "", ErrIntegrity
	}
	for index, name := range legacyNativeEventRecordFieldNames {
		if legacyEventNativeFields.fields[index].name != name {
			return "", ErrIntegrity
		}
	}
	return legacyNativeCheckpointModelSchema(&legacyEventNativeFields)
}

type legacyNativeEventRecordFieldsState struct {
	Version             uint8                     `json:"v"`
	Schema              ContentHash               `json:"schema"`
	Binding             ContentHash               `json:"binding"`
	Values              legacyNativeCheckpointRef `json:"values"`
	Sequence            int                       `json:"seq"`
	Presence            eventFieldPresence        `json:"presence"`
	CompactSummary      bool                      `json:"compact"`
	AgentMessage        bool                      `json:"agent"`
	ReplacementComplete bool                      `json:"replacement_complete"`
	HasLocked           bool                      `json:"locked"`
	HasMetadata         bool                      `json:"metadata"`
	HasCreateTime       bool                      `json:"create_time"`
	HasError            bool                      `json:"has_error"`
	Error               bool                      `json:"error"`
	HasReplayable       bool                      `json:"has_replayable"`
	Replayable          bool                      `json:"replayable"`
	BooleanAlias        bool                      `json:"boolean_alias"`
}

func (state legacyNativeEventRecordFieldsState) validate(binding ContentHash, complete bool) error {
	schema, err := legacyNativeEventRecordFieldSchema()
	if err != nil || state.Version != 1 || state.Schema != schema || state.Binding != binding || ValidateContentHash(binding) != nil || len(binding) != len(contentHashPrefix)+64 ||
		state.HasCreateTime && !state.HasMetadata || !state.HasError && state.Error || !state.HasReplayable && state.Replayable ||
		state.BooleanAlias && (!state.HasError || !state.HasReplayable || state.Error != state.Replayable) {
		return ErrIntegrity
	}
	if complete && !state.Values.valid() || !complete && state.Values != (legacyNativeCheckpointRef{}) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeEventRecordFieldsFor(event CIREvent, binding ContentHash) (legacyNativeEventRecordFieldsState, [legacyNativeEventRecordFieldRoots]any, error) {
	var roots [legacyNativeEventRecordFieldRoots]any
	state := legacyNativeEventRecordFieldsState{Version: 1, Binding: binding, Sequence: event.Seq, Presence: event.fieldPresence,
		CompactSummary: event.CompactSummary, AgentMessage: event.AgentMessage, ReplacementComplete: event.ReplacementComplete,
		HasLocked: event.Locked != nil, HasMetadata: event.ProviderMetadata != nil, HasError: event.IsError != nil, HasReplayable: event.CrossReplayable != nil}
	if event.Blocks != nil || event.Replacement != nil {
		return state, roots, ErrIntegrity
	}
	var err error
	state.Schema, err = legacyNativeEventRecordFieldSchema()
	if err != nil {
		return state, roots, err
	}
	roots = [legacyNativeEventRecordFieldRoots]any{string(event.Kind), event.ID, event.TS, string(event.Role), event.AgentAuthor, event.AgentRecipient,
		event.CallID, event.ToolName, event.ProviderToolName, event.Status, event.RedactedSummary, "", "", "", "", "", event.Input, event.Output}
	if event.Locked != nil {
		roots[11], roots[12], roots[13] = string(event.Locked.Provider), event.Locked.Scheme, event.Locked.Blob
	}
	if event.ProviderMetadata != nil {
		roots[14] = event.ProviderMetadata.TurnID
		if event.ProviderMetadata.CreateTime != nil {
			state.HasCreateTime, roots[15] = true, string(*event.ProviderMetadata.CreateTime)
		}
	}
	if event.IsError != nil {
		state.Error = *event.IsError
	}
	if event.CrossReplayable != nil {
		state.Replayable = *event.CrossReplayable
	}
	state.BooleanAlias = event.IsError != nil && event.IsError == event.CrossReplayable
	return state, roots, state.validate(binding, false)
}

func (state legacyNativeEventRecordFieldsState) restore(values []legacyNativeAnyValue) (CIREvent, error) {
	var event CIREvent
	if err := state.validate(state.Binding, true); err != nil || len(values) != legacyNativeEventRecordFieldRoots {
		return event, ErrIntegrity
	}
	var text [16]string
	for index := range text {
		value, ok := values[index].value.(string)
		if !ok {
			return event, ErrIntegrity
		}
		text[index] = value
	}
	if !state.HasLocked && (text[11] != "" || text[12] != "" || text[13] != "") || !state.HasMetadata && text[14] != "" || !state.HasCreateTime && text[15] != "" {
		return event, ErrIntegrity
	}
	input, ok := values[16].value.(map[string]any)
	if !ok && values[16].value != nil {
		return event, ErrIntegrity
	}
	event = CIREvent{Kind: EventKind(text[0]), ID: text[1], TS: text[2], Seq: state.Sequence, Role: Role(text[3]), AgentAuthor: text[4], AgentRecipient: text[5],
		CallID: text[6], ToolName: text[7], ProviderToolName: text[8], Status: text[9], RedactedSummary: text[10], Input: input, Output: values[17].value,
		fieldPresence: state.Presence, CompactSummary: state.CompactSummary, AgentMessage: state.AgentMessage, ReplacementComplete: state.ReplacementComplete}
	if state.HasLocked {
		event.Locked = &LockedBlob{Provider: ProviderKind(text[11]), Scheme: text[12], Blob: text[13]}
	}
	if state.HasMetadata {
		event.ProviderMetadata = &ProviderMetadata{TurnID: text[14]}
		if state.HasCreateTime {
			number := json.Number(text[15])
			event.ProviderMetadata.CreateTime = &number
		}
	}
	if state.HasError {
		value := state.Error
		event.IsError = &value
	}
	if state.HasReplayable {
		value := state.Replayable
		event.CrossReplayable = &value
		if state.BooleanAlias {
			event.CrossReplayable = event.IsError
		}
	}
	return event, nil
}

type legacyNativeEventRecordFieldsCheckpoint struct {
	record  legacyNativeEventValue
	state   legacyNativeEventRecordFieldsState
	roots   [legacyNativeEventRecordFieldRoots]any
	graph   *legacyNativeCheckpointGoGraphSet
	capture *legacyNormalizationCapture
	store   legacyNativeCheckpointIO
	root    legacyNativeCheckpointRef
	budget  int64
	ready   bool
	closed  bool
	failure error
}

func newLegacyNativeEventRecordFieldsCheckpoint(ctx context.Context, list *legacyNativeEventList, index int, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyNativeEventRecordFieldsCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if list == nil || list.mutation == nil || store.Put == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventRecordFieldsCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.allocation(2*LegacyCanonicalEncodingBytes, 1)
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	capture, err := list.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	if !list.paged || list.values == nil || !list.values.records.valid() || list.values.count < 0 || list.values.count > list.values.records.length() || index < 0 || index >= list.values.count {
		capture.release()
		return nil, ErrIntegrity
	}
	record := list.values.records.at(index)
	if record == nil {
		capture.release()
		return nil, ErrIntegrity
	}
	planned.include(record.heap)
	if planned.err != nil || planned.bytes > budget {
		capture.release()
		if planned.err != nil {
			return nil, planned.err
		}
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeEventRecordFieldsCheckpoint{record: *record, capture: capture, store: store, budget: budget}
	if err := work.initialize(ctx, binding); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeEventRecordFieldsCheckpoint) initialize(ctx context.Context, binding ContentHash) error {
	if work.capture == nil || !work.capture.valid(work.capture.scope) {
		return ErrConflict
	}
	var err error
	work.state, work.roots, err = legacyNativeEventRecordFieldsFor(work.record.event, binding)
	if err != nil {
		return err
	}
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	work.graph, err = newLegacyNativeCheckpointGoGraphSetForSource(ctx, work.roots[:], binding, work.store, remaining, nil, work)
	return err
}

func (work *legacyNativeEventRecordFieldsCheckpoint) checkSource() error {
	if work.capture == nil || !work.capture.valid(work.capture.scope) {
		return ErrConflict
	}
	state, roots, err := legacyNativeEventRecordFieldsFor(work.record.event, work.state.Binding)
	if err != nil {
		return err
	}
	state.Values = work.state.Values
	if state != work.state {
		return ErrConflict
	}
	for index, value := range roots {
		if !legacyNativeCheckpointGoGraphSetSame(value, work.roots[index]) {
			return ErrConflict
		}
	}
	return nil
}

func (work *legacyNativeEventRecordFieldsCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	heap.include(work.record.heap)
	for _, hash := range []ContentHash{work.state.Schema, work.state.Binding, work.state.Values.Hash, work.root.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	if work.graph != nil {
		retained, err := work.graph.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventRecordFieldsCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeEventRecordFieldsCheckpoint) step(ctx context.Context) (bool, error) {
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
	if _, err := work.remaining(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if work.graph != nil {
		if !work.graph.ready {
			if _, err := work.graph.step(ctx); err != nil {
				return work.fail(err)
			}
			return false, nil
		}
		var err error
		work.state.Values, err = work.graph.take(ctx)
		work.graph = nil
		if err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	remaining, err := work.remaining()
	if err != nil {
		return work.fail(err)
	}
	if remaining < 2*LegacyCanonicalEncodingBytes {
		return work.fail(ErrDocumentResourceLimit)
	}
	work.root, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
	if err != nil {
		return work.fail(err)
	}
	if _, err := work.remaining(); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return true, nil
}

func (work *legacyNativeEventRecordFieldsCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	root := work.root
	work.close()
	return root, nil
}

func (work *legacyNativeEventRecordFieldsCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventRecordFieldsCheckpoint) close() {
	if work != nil && !work.closed {
		work.graph.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNativeEventRecordFieldsCheckpoint{closed: true, failure: failure}
	}
}

func (state legacyNativeEventRecordFieldsState) pointerHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if state.HasLocked {
		heap.allocation(1, int64(unsafe.Sizeof(LockedBlob{})))
	}
	if state.HasMetadata {
		heap.allocation(1, int64(unsafe.Sizeof(ProviderMetadata{})))
	}
	if state.HasError {
		heap.allocation(1, int64(unsafe.Sizeof(false)))
	}
	if state.HasReplayable && !state.BooleanAlias {
		heap.allocation(1, int64(unsafe.Sizeof(false)))
	}
	if state.HasCreateTime {
		heap.allocation(1, int64(unsafe.Sizeof(json.Number(""))))
	}
	return heap
}

func (state legacyNativeEventRecordFieldsState) payload(event CIREvent, values []legacyNativeAnyValue) (docRangeHeaderHeapEstimate, int64) {
	heap := state.pointerHeap()
	var excess docRangeHeaderHeapEstimate
	for _, value := range values[:16] {
		text, ok := value.value.(string)
		if !ok || value.validateOwnership() != nil {
			heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
			return heap, 0
		}
		heap.allocation(int64(value.capacity), 1)
		excess.include(docRangeHeaderHeapEstimate{bytes: 2 * int64(value.capacity-len(text))})
	}
	if event.Input != nil {
		heap.include(values[16].heap)
		excess.include(docRangeHeaderHeapEstimate{bytes: values[16].excess})
	}
	if event.Output != nil {
		heap.include(values[17].heap)
		excess.include(docRangeHeaderHeapEstimate{bytes: values[17].excess})
	}
	if excess.err != nil || excess.bytes > heap.bytes {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
	}
	return heap, excess.bytes
}

type legacyNativeEventRecordFieldsRestored struct {
	Event  CIREvent
	Heap   docRangeHeaderHeapEstimate
	Excess int64
}

type legacyNativeEventRecordFieldsRestore struct {
	state      legacyNativeEventRecordFieldsState
	ref        legacyNativeCheckpointRef
	binding    ContentHash
	store      legacyNativeCheckpointIO
	graph      *legacyNativeCheckpointGoGraphSetRestore
	createTime *string
	result     legacyNativeEventRecordFieldsRestored
	budget     int64
	loaded     bool
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeEventRecordFieldsRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyNativeEventRecordFieldsRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventRecordFieldsRestore{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	planned.allocation(int64(len(binding)), 1)
	planned.include(docRangeHeaderHeapEstimate{bytes: 2 * LegacyCanonicalEncodingBytes})
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeEventRecordFieldsRestore{ref: ref, binding: binding, store: store, budget: budget}
	if remaining, err := work.remaining(); err != nil || remaining < 2*LegacyCanonicalEncodingBytes {
		work.close()
		if err != nil {
			return nil, err
		}
		return nil, ErrDocumentResourceLimit
	}
	return work, nil
}

func (work *legacyNativeEventRecordFieldsRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	for _, hash := range []ContentHash{work.ref.Hash, work.binding, work.state.Schema, work.state.Binding, work.state.Values.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	heap.include(work.result.Heap)
	if work.graph != nil {
		retained, err := work.graph.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventRecordFieldsRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeEventRecordFieldsRestore) step(ctx context.Context) (bool, error) {
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
	remaining, err := work.remaining()
	if err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if !work.loaded {
		if remaining < 2*LegacyCanonicalEncodingBytes {
			return work.fail(ErrDocumentResourceLimit)
		}
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return work.fail(err)
		}
		if err := work.state.validate(work.binding, true); err != nil {
			return work.fail(err)
		}
		work.loaded = true
		return false, nil
	}
	if work.graph == nil {
		pointers := work.state.pointerHeap()
		if pointers.err != nil || pointers.bytes >= remaining {
			return work.fail(ErrDocumentResourceLimit)
		}
		work.graph, err = newLegacyNativeCheckpointGoGraphSetRestore(ctx, work.state.Values, work.binding, work.store, remaining-pointers.bytes)
		if err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if !work.graph.ready {
		if _, err := work.graph.step(ctx); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	values, err := work.graph.take(ctx)
	work.graph = nil
	if err != nil {
		return work.fail(err)
	}
	work.result.Heap = legacyNativeObjectInitialValuesHeap(values)
	work.result.Event, err = work.state.restore(values)
	if err != nil {
		return work.fail(err)
	}
	if work.createTime != nil {
		metadata := work.result.Event.ProviderMetadata
		if metadata == nil || metadata.CreateTime == nil || len(*metadata.CreateTime) != len(*work.createTime) {
			return work.fail(ErrIntegrity)
		}
		*metadata.CreateTime = json.Number(*work.createTime)
	}
	work.result.Heap, work.result.Excess = work.state.payload(work.result.Event, values)
	if _, err := work.remaining(); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return true, nil
}

func (work *legacyNativeEventRecordFieldsRestore) take(ctx context.Context) (legacyNativeEventRecordFieldsRestored, error) {
	if work == nil {
		return legacyNativeEventRecordFieldsRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeEventRecordFieldsRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeEventRecordFieldsRestored{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeEventRecordFieldsRestored{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeEventRecordFieldsRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventRecordFieldsRestore) close() {
	if work != nil && !work.closed {
		work.graph.close()
		failure := work.failure
		*work = legacyNativeEventRecordFieldsRestore{closed: true, failure: failure}
	}
}
