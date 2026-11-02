package domain

import (
	"context"
	"encoding/json"
	"unsafe"
)

const legacyNativeEventRecordScalarPairBytes = 16 << 10

func legacyNativeEventRecordScalarPairEligible(event CIREvent) bool {
	if event.ProviderMetadata != nil || event.Locked != nil || event.Input != nil || event.Blocks != nil || event.Replacement != nil {
		return false
	}
	if event.Output != nil {
		output, ok := event.Output.(map[string]any)
		if !ok || output != nil {
			return false
		}
	}
	for _, value := range [...]string{string(event.Kind), event.ID, event.TS, string(event.Role), event.AgentAuthor,
		event.AgentRecipient, event.CallID, event.ToolName, event.ProviderToolName, event.Status, event.RedactedSummary} {
		if len(value) > 64 {
			return false
		}
	}
	return true
}

func legacyNativeEventRecordScalarPairWorkspace() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventRecordFieldsCheckpoint{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphSetState{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeValueSnapshot{})))
	heap.allocation(legacyNativeEventRecordFieldRoots, int64(unsafe.Sizeof(legacyNativeCheckpointValue{}))+legacyNativeValueCheckpointBytes+64)
	heap.allocation(4*legacyNativeEventRecordScalarPairBytes, 1)
	heap.allocation(int64(4*(len(contentHashPrefix)+64)), 1)
	return heap
}

func (work *legacyNativeEventValuesCheckpoint) checkScalarRecordPair(ctx context.Context, frame *legacyNativeEventValuesCheckpointFrame, fields *legacyNativeEventRecordFieldsCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.checkSource(); err != nil {
		return err
	}
	if work.current != frame || frame.phase != 2 || work.fields != nil || frame.source == nil ||
		frame.index < 0 || frame.index >= frame.source.records.length() || fields.capture != work.capture || fields.state.Binding != work.state.Binding {
		return ErrConflict
	}
	record := frame.source.records.at(frame.index)
	if record == nil || record.heap != frame.record.heap || record.excess != frame.record.excess ||
		record.blocks != frame.record.blocks || record.replacement != frame.record.replacement {
		return ErrConflict
	}
	fields.record = *record
	if err := fields.checkSource(); err != nil {
		return err
	}
	return work.admit(0)
}

func (work *legacyNativeEventValuesCheckpoint) scalarRecordPair(ctx context.Context) (bool, error) {
	frame := work.current
	if !legacyNativeEventRecordScalarPairEligible(frame.record.event) {
		return false, nil
	}
	workspace := legacyNativeEventRecordScalarPairWorkspace()
	if workspace.err != nil {
		return false, workspace.err
	}
	if err := work.admit(workspace.bytes); err != nil {
		return false, err
	}
	work.scalarPairHeap = workspace.bytes
	defer func() { work.scalarPairHeap = 0 }()
	fields := legacyNativeEventRecordFieldsCheckpoint{record: frame.record, capture: work.capture}
	var err error
	fields.state, fields.roots, err = legacyNativeEventRecordFieldsFor(frame.record.event, work.state.Binding)
	if err != nil {
		return false, err
	}
	if err := work.checkScalarRecordPair(ctx, frame, &fields); err != nil {
		return false, err
	}
	graph := legacyNativeCheckpointGoGraphSetState{Version: 1, Binding: fields.state.Binding,
		Roots: make(legacyNativeCheckpointGoGraphRoots, legacyNativeEventRecordFieldRoots)}
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
	for index, root := range fields.roots {
		value := legacyNativeAnyValue{value: root}
		if text, ok := root.(string); ok {
			value.capacity = len(text)
		}
		snapshot, err := value.checkpoint(ctx, 0, bounds)
		if err != nil {
			return false, err
		}
		wire, err := snapshot.state.encode(ctx, bounds)
		if err != nil {
			snapshot.close()
			return false, err
		}
		graph.Roots[index] = legacyNativeCheckpointValue{Wire: wire}
		if snapshot.state.Kind == legacyNativeCheckpointString && snapshot.state.Length > 0 {
			graph.Roots[index].Inline = []byte(snapshot.text)
		}
		snapshot.close()
	}
	if err := graph.validate(fields.state.Binding); err != nil {
		return false, err
	}
	graphBody, err := json.Marshal(graph)
	if err != nil || len(graphBody) > legacyNativeEventRecordScalarPairBytes {
		return false, ErrIntegrity
	}
	graphHash, err := HashContentContext(ctx, graphBody)
	if err != nil {
		return false, err
	}
	fields.state.Values = legacyNativeCheckpointRef{Hash: graphHash, Bytes: len(graphBody)}
	if err := fields.state.validate(fields.state.Binding, true); err != nil {
		return false, err
	}
	fieldsBody, err := json.Marshal(fields.state)
	if err != nil || len(graphBody)+len(fieldsBody) > legacyNativeEventRecordScalarPairBytes {
		return false, ErrIntegrity
	}
	if err := work.checkScalarRecordPair(ctx, frame, &fields); err != nil {
		return false, err
	}
	graphRef, err := legacyNativeCheckpointWrite(ctx, work.store, graphBody)
	if err != nil {
		return false, err
	}
	if graphRef != fields.state.Values {
		return false, ErrIntegrity
	}
	if err := work.checkScalarRecordPair(ctx, frame, &fields); err != nil {
		return false, err
	}
	ref, err := legacyNativeCheckpointWrite(ctx, work.store, fieldsBody)
	if err != nil {
		return false, err
	}
	if err := work.checkScalarRecordPair(ctx, frame, &fields); err != nil {
		return false, err
	}
	frame.fields, frame.phase = ref, 3
	return true, nil
}
