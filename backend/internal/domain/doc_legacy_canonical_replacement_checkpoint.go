package domain

import (
	"context"
	"strconv"
	"unsafe"
)

type legacyCanonicalReplacementState struct {
	Kind    string                            `json:"kind"`
	Version uint8                             `json:"version"`
	Binding ContentHash                       `json:"binding"`
	Order   legacyCanonicalPreparedOrderState `json:"order"`
	Entries legacyNativeCheckpointTree        `json:"entries"`
}

func legacyCanonicalReplacementOrderValid(order legacyCanonicalKeyOrder, fields legacyCanonicalKeyAccess) bool {
	return order.valid(fields) && (order.count < 2 || order.phase != legacyCanonicalKeyOrderDone) && order.compareOffset < 32
}

type legacyCanonicalReplacementCheckpoint struct {
	owner       *LegacyNormalizationPreparation
	encoding    *LegacyCanonicalEncoding
	nativeOwner *legacyNativeCanonicalOwner
	preparation *legacyCanonicalPreparation
	frame       *legacyCanonicalPreparationFrame
	keys        *legacyCanonicalEncodingKeys
	source      *legacyCanonicalReplacement
	native      *legacyNativeEventValues
	grant       *legacyNormalizationCapture
	store       legacyNativeCheckpointIO
	state       legacyCanonicalReplacementState
	entries     legacyNativeCheckpointListWriter[uint64]
	ref         legacyNativeCheckpointRef
	budget      int64
	frameIndex  int
	position    int
	phase       uint8
	ready       bool
	closed      bool
	failure     error
}

func newLegacyCanonicalReplacementCheckpoint(ctx context.Context, owner *LegacyNormalizationPreparation, index int, grant *legacyNormalizationCapture, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalReplacementCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == nil || grant == nil || !grant.valid(owner.mutation) {
		return nil, ErrConflict
	}
	if owner.terminal != nil || owner.phase != legacyNormalizationStage || owner.next < 0 || owner.next >= len(owner.pending) ||
		ValidateContentHash(binding) != nil || store.Put == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	encoding := owner.pending[owner.next].Encoding
	if encoding == nil || encoding.nativeOwner == nil || encoding.preparation == nil || index < 0 || index >= encoding.preparation.frames.length() {
		return nil, ErrIntegrity
	}
	frame := encoding.preparation.frames.at(index)
	source, valid := frame.value.(*legacyCanonicalReplacement)
	keys := encoding.mapKeys.at(frame.mapPosition)
	if !valid || source == nil || source.native == nil || source.events != nil || source.length() != frame.count ||
		frame.kind != 'r' || frame.phase != 1 || frame.position != 0 || !frame.prepare ||
		keys == nil || keys.source != source || keys.sorted || keys.endKeys != 0 || !keys.fields.valid() || !legacyCanonicalReplacementOrderValid(source.order, &keys.fields) {
		return nil, ErrIntegrity
	}
	work := &legacyCanonicalReplacementCheckpoint{owner: owner, encoding: encoding, nativeOwner: encoding.nativeOwner, preparation: encoding.preparation, frame: frame,
		keys: keys, source: source, native: source.native, grant: grant, store: store, budget: budget, frameIndex: index,
		state: legacyCanonicalReplacementState{Kind: "replacement-order", Version: legacyNativeCheckpointVersion, Binding: binding,
			Order: legacyCanonicalPreparedOrder(source.order)}}
	if err := work.admit(); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalReplacementCheckpoint) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil || work.closed || work.owner == nil || work.grant == nil || !work.grant.valid(work.owner.mutation) {
		return ErrConflict
	}
	if work.owner.terminal != nil || work.owner.phase != legacyNormalizationStage || work.owner.next < 0 || work.owner.next >= len(work.owner.pending) ||
		work.owner.pending[work.owner.next].Encoding != work.encoding || work.encoding.nativeOwner != work.nativeOwner || work.encoding.preparation != work.preparation ||
		work.preparation.frames.at(work.frameIndex) != work.frame || work.frame.value != work.source ||
		work.frame.kind != 'r' || work.frame.phase != 1 || work.frame.position != 0 || !work.frame.prepare ||
		work.frame.count != work.state.Order.Count || work.encoding.mapKeys.at(work.frame.mapPosition) != work.keys ||
		work.source.native != work.native || work.source.events != nil || work.source.length() != work.frame.count ||
		work.source.order != work.state.Order.value() || work.keys.source != work.source || work.keys.sorted || work.keys.endKeys != 0 ||
		!work.keys.fields.valid() || work.keys.fields.length() != work.frame.count {
		return ErrConflict
	}
	return nil
}

func (work *legacyCanonicalReplacementCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Entries.Ref.Hash)+len(work.ref.Hash)), 1)
	retained, err := work.entries.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyCanonicalReplacementCheckpoint) admit() error {
	retained, err := work.retainedBytes()
	if err != nil || retained > work.budget || legacyNativeAnyCheckpointScratch > work.budget-retained {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyCanonicalReplacementCheckpoint) step(ctx context.Context) (ready bool, failure error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	defer func() {
		if failure != nil {
			work.close()
			work.failure = failure
		}
	}()
	if err := work.current(ctx); err != nil {
		return false, err
	}
	if err := work.admit(); err != nil {
		return false, err
	}
	if work.ready {
		return true, nil
	}
	if work.position < work.state.Order.Count {
		field := work.keys.fields.at(work.position)
		if field == nil || len(field.key) != 32 || field.childKeys != 0 {
			return false, ErrIntegrity
		}
		ordinal, err := strconv.ParseUint(field.key[16:], 16, 64)
		if err != nil || ordinal >= uint64(work.state.Order.Count) {
			return false, ErrIntegrity
		}
		event, _, err := work.source.at(int(ordinal))
		if err != nil || field.key != legacyCanonicalReplacementKey(event.Seq, int(ordinal)) {
			return false, ErrIntegrity
		}
		if err := work.entries.append(ctx, work.store, ordinal); err != nil {
			return false, err
		}
		work.position++
		return false, nil
	}
	if work.phase == 0 {
		entries, err := work.entries.finish(ctx, work.store)
		if err != nil {
			return false, err
		}
		work.state.Entries, work.phase = entries, 1
		return false, nil
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
	if err != nil {
		return false, err
	}
	if err := work.current(ctx); err != nil {
		return false, err
	}
	work.ref, work.ready = ref, true
	return true, nil
}

func (work *legacyCanonicalReplacementCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if err := work.current(ctx); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if err := work.admit(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if !work.ready {
		return legacyNativeCheckpointRef{}, ErrDocRangeIndexPending
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyCanonicalReplacementCheckpoint) close() {
	if work != nil && !work.closed {
		work.entries.close()
		*work = legacyCanonicalReplacementCheckpoint{closed: true, failure: work.failure}
	}
}
