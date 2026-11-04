package domain

import "context"

type legacyCanonicalSourceReference struct {
	Record uint64                      `json:"record"`
	Graph  legacyNativeCheckpointRef   `json:"graph"`
	Body   legacyNativeCheckpointRef   `json:"body"`
	Value  legacyNativeValueCheckpoint `json:"value"`
}

func (reference legacyCanonicalSourceReference) validate() error {
	if reference.Record == 0 || reference.Record > 8*uint64(MaxFinalizedDocBytes) || !reference.Graph.valid() || !reference.Body.valid() ||
		reference.Value.Nil || reference.Value.Kind != legacyNativeCheckpointObject && reference.Value.Kind != legacyNativeCheckpointArray {
		return ErrIntegrity
	}
	return reference.Value.validate(legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes})
}

func (work *legacyCanonicalSourceCheckpoint) sourceGraph() *legacyNativeCheckpointGoGraphSet {
	if work == nil || work.closed || work.failure != nil || work.child == nil || work.child.current == nil || work.child.current.phase != 2 || work.child.fields == nil {
		return nil
	}
	graph := work.child.fields.graph
	if graph == nil || graph.closed || graph.failure != nil || !graph.ready {
		return nil
	}
	return graph
}

func (work *legacyCanonicalSourceCheckpoint) sourceReference(ctx context.Context, value any) (legacyCanonicalSourceReference, bool, error) {
	var reference legacyCanonicalSourceReference
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
	if err := work.admit(0); err != nil {
		return reference, false, err
	}
	if graph.registry == nil || graph.registry.closed || graph.registry.failure != nil || graph.identity == nil || graph.identity.closed || graph.identity.failure != nil {
		return reference, false, ErrIntegrity
	}
	key, _, _, err := legacyNativeCheckpointGoGraphDescribe(ctx, value)
	if err != nil || key.kind == 0 {
		return reference, false, err
	}
	entry, err := graph.registry.lookup(key)
	if err != nil {
		return reference, false, err
	}
	if entry == nil || entry.key != key {
		return reference, false, nil
	}
	if !entry.complete || !legacyNativeCheckpointGoGraphSetSame(entry.source, value) {
		return reference, false, ErrConflict
	}
	if err := entry.value.validateOwnership(); err != nil {
		return reference, false, err
	}
	address, err := legacyNativeCheckpointGraphOwnerAddress(entry.value.owner)
	if err != nil {
		return reference, false, err
	}
	captured, missing := graph.identity.lookup(address)
	if missing != 0 || captured == nil || !captured.finished || captured.owner != entry.value.owner {
		return reference, false, ErrIntegrity
	}
	if err := captured.validate(); err != nil {
		return reference, false, err
	}
	rows := &work.child.rows
	limit := 8 * uint64(work.child.state.SourceBytes)
	if rows.closed || rows.failure != nil || rows.done || rows.tree.closed || rows.tree.failure != nil || rows.tree.done ||
		rows.tree.count > limit || uint64(len(rows.values)) > limit-rows.tree.count {
		return reference, false, ErrIntegrity
	}
	reference = legacyCanonicalSourceReference{Record: rows.tree.count + uint64(len(rows.values)), Graph: graph.ref, Body: captured.body, Value: captured.state}
	if err := reference.validate(); err != nil {
		return legacyCanonicalSourceReference{}, false, err
	}
	return reference, true, nil
}

func (work *legacyCanonicalInitialRestore) sourceGraph() *legacyNativeCheckpointGoGraphSetRestore {
	if work == nil || work.closed || work.failure != nil || work.child == nil || work.child.current == nil || work.child.current.phase != 2 || work.child.fields == nil {
		return nil
	}
	graph := work.child.fields.graph
	if graph == nil || graph.closed || graph.failure != nil || !graph.ready {
		return nil
	}
	return graph
}

func (work *legacyCanonicalInitialRestore) resolveSourceReference(ctx context.Context, reference legacyCanonicalSourceReference) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := reference.validate(); err != nil {
		return nil, false, err
	}
	graph := work.sourceGraph()
	if graph == nil {
		return nil, false, ErrIntegrity
	}
	if err := work.admit(0); err != nil {
		return nil, false, err
	}
	rows := work.child.rows
	if rows == nil || rows.closed || rows.failure != nil || rows.cursor == nil || rows.cursor.closed || rows.cursor.failure != nil ||
		rows.index <= 0 || rows.index > len(rows.values) || rows.cursor.read < uint64(len(rows.values)) || rows.cursor.read > rows.cursor.want {
		return nil, false, ErrIntegrity
	}
	position := rows.cursor.read - uint64(len(rows.values)) + uint64(rows.index-1)
	if reference.Record != position {
		return nil, false, nil
	}
	if reference.Graph != graph.ref || graph.state.Binding != work.child.binding || graph.registry == nil || graph.registry.closed || graph.registry.failure != nil {
		return nil, false, ErrIntegrity
	}
	entry, missing := graph.registry.lookup(reference.Value.Reference)
	if missing != 0 || entry == nil || entry.active || entry.body != reference.Body {
		return nil, false, ErrIntegrity
	}
	state, err := decodeLegacyNativeValueCheckpoint(ctx, entry.wire[:], legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes})
	if err != nil || state != reference.Value {
		return nil, false, ErrIntegrity
	}
	if err := entry.value.validateOwnership(); err != nil {
		return nil, false, err
	}
	return entry.value.value, true, nil
}
