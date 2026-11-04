package domain

import (
	"context"
	"unsafe"
)

const legacyCanonicalSourceNumberRoot = 15

type legacyCanonicalSourceNumberReference struct {
	Record  uint64                     `json:"record"`
	Graph   legacyNativeCheckpointRef  `json:"graph"`
	Ordinal int                        `json:"ordinal"`
	Text    legacyNativeCheckpointText `json:"text"`
}

func (reference legacyCanonicalSourceNumberReference) validate() error {
	if reference.Record == 0 || reference.Record > 8*uint64(MaxFinalizedDocBytes) || !reference.Graph.valid() ||
		reference.Ordinal < 0 || uint64(reference.Ordinal) >= legacyNativeValueCheckpointPayloadLimit() {
		return ErrIntegrity
	}
	return (legacyNativeCheckpointStringValueRow{Capacity: max(1, reference.Text.Bytes), Text: reference.Text}).validate()
}

func legacyCanonicalSourceNumberDescriptor(ctx context.Context, saved legacyNativeCheckpointValue, text legacyNativeCheckpointText) error {
	if saved.Text != text {
		return ErrIntegrity
	}
	state, err := legacyNativeCheckpointGraphRestoreDescriptor(ctx, saved)
	if err != nil {
		return err
	}
	if state.Kind != legacyNativeCheckpointString {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyCanonicalSourceCheckpoint) sourceNumberReference(ctx context.Context, texts *legacyCanonicalSourceNumberTexts) (legacyCanonicalSourceNumberReference, bool, error) {
	var reference legacyCanonicalSourceNumberReference
	if err := ctx.Err(); err != nil {
		return reference, false, err
	}
	graph := work.sourceGraph()
	if graph == nil {
		return reference, false, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		return reference, false, err
	}
	if err := graph.checkSource(); err != nil {
		return reference, false, err
	}
	if err := work.admit(int64(unsafe.Sizeof(reference))); err != nil {
		return reference, false, err
	}
	numbers := &work.encoding.numbers
	if err := texts.validate(numbers, work.child.state.Binding); err != nil {
		return reference, false, err
	}
	fields := work.child.fields
	if graph.state.Binding != work.child.state.Binding || graph.record != fields ||
		len(graph.state.Roots) != legacyNativeEventRecordFieldRoots || len(graph.values) != legacyNativeEventRecordFieldRoots {
		return reference, false, ErrIntegrity
	}
	if !fields.state.HasCreateTime {
		return reference, false, nil
	}
	metadata := fields.record.event.ProviderMetadata
	graphText, ok := graph.values[legacyCanonicalSourceNumberRoot].(string)
	if !ok || metadata == nil || metadata.CreateTime == nil {
		return reference, false, ErrIntegrity
	}
	text := string(*metadata.CreateTime)
	if len(graphText) != len(text) || len(text) != 0 && legacyCanonicalNumberKey(graphText) != legacyCanonicalNumberKey(text) {
		return reference, false, ErrIntegrity
	}
	entry, err := numbers.proven.lookupOptional(legacyCanonicalNumberKey(text))
	if err != nil || entry == nil {
		return reference, false, err
	}
	pending := numbers.pending.at(entry.ordinal)
	saved := texts.values.at(entry.ordinal)
	if pending == nil || saved == nil || legacyCanonicalNumberKey(*pending) != entry.identity ||
		entry.proven != (entry.ordinal < numbers.next) || saved.text.Bytes != len(text) {
		return reference, false, ErrIntegrity
	}
	if err := legacyCanonicalSourceNumberDescriptor(ctx, graph.state.Roots[legacyCanonicalSourceNumberRoot], saved.text); err != nil {
		return reference, false, err
	}
	rows := &work.child.rows
	limit := 8 * uint64(work.child.state.SourceBytes)
	if rows.closed || rows.failure != nil || rows.done || rows.tree.closed || rows.tree.failure != nil || rows.tree.done ||
		rows.tree.count > limit || uint64(len(rows.values)) > limit-rows.tree.count {
		return reference, false, ErrIntegrity
	}
	reference = legacyCanonicalSourceNumberReference{Record: rows.tree.count + uint64(len(rows.values)), Graph: graph.ref, Ordinal: entry.ordinal, Text: saved.text}
	if err := reference.validate(); err != nil {
		return legacyCanonicalSourceNumberReference{}, false, err
	}
	return reference, true, nil
}

func (work *legacyCanonicalInitialRestore) resolveSourceNumberReference(ctx context.Context, reference legacyCanonicalSourceNumberReference, numbers *legacyCanonicalNumberValidation, texts *legacyCanonicalSourceNumberTexts) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := reference.validate(); err != nil {
		return false, err
	}
	graph := work.sourceGraph()
	if graph == nil {
		return false, ErrIntegrity
	}
	if err := work.admit(0); err != nil {
		return false, err
	}
	if err := graph.admit(0); err != nil {
		return false, err
	}
	if err := texts.validate(numbers, work.child.binding); err != nil {
		return false, err
	}
	rows := work.child.rows
	if rows == nil || rows.closed || rows.failure != nil || rows.cursor == nil || rows.cursor.closed || rows.cursor.failure != nil ||
		rows.index <= 0 || rows.index > len(rows.values) || rows.cursor.read < uint64(len(rows.values)) || rows.cursor.read > rows.cursor.want {
		return false, ErrIntegrity
	}
	position := rows.cursor.read - uint64(len(rows.values)) + uint64(rows.index-1)
	if reference.Record != position {
		return false, nil
	}
	fields := work.child.fields
	if reference.Graph != graph.ref || graph.state.Binding != work.child.binding || fields.state.Values != graph.ref ||
		!fields.state.HasMetadata || !fields.state.HasCreateTime ||
		len(graph.state.Roots) != legacyNativeEventRecordFieldRoots || len(graph.values) != legacyNativeEventRecordFieldRoots {
		return false, ErrIntegrity
	}
	pending := numbers.pending.at(reference.Ordinal)
	saved := texts.values.at(reference.Ordinal)
	if pending == nil || saved == nil || reference.Text != saved.text || reference.Text.Bytes != len(*pending) || saved.capacity < max(1, len(*pending)) {
		return false, ErrIntegrity
	}
	ordinal, err := numbers.index(*pending)
	if err != nil || ordinal != reference.Ordinal {
		return false, ErrIntegrity
	}
	if err := (legacyNativeCheckpointStringValueRow{Capacity: saved.capacity, Text: saved.text}).validate(); err != nil {
		return false, err
	}
	if err := legacyCanonicalSourceNumberDescriptor(ctx, graph.state.Roots[legacyCanonicalSourceNumberRoot], saved.text); err != nil {
		return false, err
	}
	value := &graph.values[legacyCanonicalSourceNumberRoot]
	text, ok := value.value.(string)
	if !ok || len(text) != saved.text.Bytes {
		return false, ErrIntegrity
	}
	if err := value.validateOwnership(); err != nil {
		return false, err
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof("")))
	heap.allocation(int64(saved.capacity), 1)
	if heap.err != nil {
		return false, heap.err
	}
	planned := docRangeHeaderHeapEstimate{bytes: max(int64(0), heap.bytes-value.heap.bytes)}
	planned.allocation(1, int64(unsafe.Sizeof("")))
	if planned.err != nil {
		return false, planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return false, err
	}
	if err := graph.admit(planned.bytes); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	value.value = *pending
	value.heap, value.capacity, value.excess = heap, saved.capacity, 2*int64(saved.capacity-len(*pending))
	fields.createTime = pending
	return true, nil
}
