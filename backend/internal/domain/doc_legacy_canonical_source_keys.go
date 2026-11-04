package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalSourceKeyReference struct {
	Source legacyCanonicalSourceReference `json:"source"`
	Member uint64                         `json:"member"`
	Text   legacyNativeCheckpointText     `json:"text"`
}

func (reference legacyCanonicalSourceKeyReference) validate() error {
	if err := reference.Source.validate(); err != nil {
		return err
	}
	if reference.Source.Value.Kind != legacyNativeCheckpointObject || reference.Member >= reference.Source.Value.Length {
		return ErrIntegrity
	}
	text := reference.Text
	if text.Bytes < 0 || uint64(text.Bytes) > legacyNativeValueCheckpointPayloadLimit() ||
		!text.Tree.valid() || text.Tree.Count != uint64(text.Bytes) ||
		(text.Bytes == 0) != (text.Tree == (legacyNativeCheckpointTree{})) {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyCanonicalInitialRestore) sourceKey(ctx context.Context) (legacyCanonicalSourceKeyReference, string, bool, error) {
	var reference legacyCanonicalSourceKeyReference
	if work == nil {
		return reference, "", false, ErrIntegrity
	}
	if work.failure != nil {
		return reference, "", false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return reference, "", false, err
	}
	if work.closed {
		return reference, "", false, ErrIntegrity
	}
	child := work.child
	if child == nil || child.current == nil || child.current.phase != 2 || child.fields == nil || child.fields.graph == nil {
		return reference, "", false, nil
	}
	fields := child.fields
	graph := fields.graph
	read := graph.read
	if read == nil || read.frame == nil || read.frame.phase != legacyNativeCheckpointGraphRestoreValue || read.frame.state.Kind != legacyNativeCheckpointObject {
		return reference, "", false, nil
	}
	frame := read.frame
	if child.closed || child.failure != nil || fields.closed || fields.failure != nil || fields.ready ||
		graph.closed || graph.failure != nil || graph.ready || read.closed || read.failure != nil || read.ready ||
		graph.registry == nil || graph.registry.closed || graph.registry.failure != nil || read.registry != graph.registry ||
		fields.binding != child.binding || graph.binding != child.binding || graph.state.Binding != child.binding ||
		fields.ref != child.current.fields || graph.ref != fields.state.Values {
		return reference, "", false, ErrIntegrity
	}
	if err := work.admit(0); err != nil {
		return reference, "", false, err
	}
	if err := child.admit(0); err != nil {
		return reference, "", false, err
	}
	if _, err := fields.remaining(); err != nil {
		return reference, "", false, err
	}
	if err := graph.admit(0); err != nil {
		return reference, "", false, err
	}
	if err := read.admit(0, 0); err != nil {
		return reference, "", false, err
	}
	rows := child.rows
	if rows == nil || rows.closed || rows.failure != nil || rows.cursor == nil || rows.cursor.closed || rows.cursor.failure != nil ||
		rows.index <= 0 || rows.index > len(rows.values) || rows.cursor.read < uint64(len(rows.values)) || rows.cursor.read > rows.cursor.want {
		return reference, "", false, ErrIntegrity
	}
	record := rows.cursor.read - uint64(len(rows.values)) + uint64(rows.index-1)
	if record != child.current.recordRow || record > 8*uint64(child.sourceBytes) {
		return reference, "", false, ErrIntegrity
	}
	entry, missing := graph.registry.lookup(frame.state.Reference)
	if missing != 0 || entry == nil || entry != frame.entry || !entry.active || entry.identity != frame.state.Reference ||
		entry.object == nil || entry.array != nil || frame.record.Identity != entry.identity || frame.record.Kind != legacyNativeCheckpointObject ||
		frame.record.Length != frame.state.Length || frame.position >= frame.record.Length {
		return reference, "", false, ErrIntegrity
	}
	state, err := decodeLegacyNativeValueCheckpoint(ctx, entry.wire[:], legacyNativeValueCheckpointBounds{
		PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes,
	})
	if err != nil || state != frame.state {
		return reference, "", false, ErrIntegrity
	}
	members := frame.members
	if members == nil || members.closed || members.failure != nil || members.cursor == nil || members.cursor.closed || members.cursor.failure != nil ||
		members.index <= 0 || members.index > len(members.values) || members.cursor.read < uint64(len(members.values)) ||
		members.cursor.read > members.cursor.want || members.cursor.want != frame.record.Length ||
		members.cursor.read-uint64(len(members.values))+uint64(members.index-1) != frame.position {
		return reference, "", false, ErrIntegrity
	}
	keyState, err := legacyNativeCheckpointGraphRestoreDescriptor(ctx, frame.member.Key)
	if err != nil {
		return reference, "", false, err
	}
	key, ok := frame.key.value.(string)
	if !ok || frame.member.Key.Inline != nil || keyState.Kind != legacyNativeCheckpointString || uint64(len(key)) != keyState.Length ||
		uint64(frame.key.capacity) != keyState.Capacity && keyState.Capacity != keyState.Length {
		return reference, "", false, ErrIntegrity
	}
	if err := frame.key.validateOwnership(); err != nil {
		return reference, "", false, err
	}
	reference = legacyCanonicalSourceKeyReference{
		Source: legacyCanonicalSourceReference{Record: record, Graph: graph.ref, Body: entry.body, Value: state},
		Member: frame.position, Text: frame.member.Key.Text,
	}
	if err := reference.validate(); err != nil {
		return legacyCanonicalSourceKeyReference{}, "", false, err
	}
	if err := ctx.Err(); err != nil {
		return legacyCanonicalSourceKeyReference{}, "", false, err
	}
	return reference, key, true, nil
}

func (work *legacyCanonicalInitialRestore) stepSourceKeys(ctx context.Context, bind func(context.Context, legacyCanonicalSourceKeyReference, string) error) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if bind == nil {
		return work.fail(ErrIntegrity)
	}
	work.read = false
	for transitions := 0; transitions < legacyCanonicalSourceCheckpointTransitions; transitions++ {
		ready, err := work.stepOnce(ctx)
		if err != nil || ready {
			return ready, err
		}
		reference, key, found, err := work.sourceKey(ctx)
		if err != nil {
			return work.fail(err)
		}
		if found {
			if err := bind(ctx, reference, key); err != nil {
				return work.fail(err)
			}
			current, borrowed, present, err := work.sourceKey(ctx)
			if err != nil {
				return work.fail(err)
			}
			if !present || current != reference || len(borrowed) != len(key) || unsafe.StringData(borrowed) != unsafe.StringData(key) {
				return work.fail(ErrConflict)
			}
			return false, nil
		}
		if work.read || work.sourceGraph() != nil || work.sourceRecord() != nil {
			return false, nil
		}
	}
	return false, nil
}
