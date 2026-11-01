package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalReplacementRestored struct {
	fields     legacyCanonicalPages[legacyCanonicalKeyField]
	order      legacyCanonicalKeyOrder
	projection docRangeHeaderHeapEstimate
}

type legacyCanonicalReplacementRestore struct {
	source  *legacyCanonicalReplacement
	native  *legacyNativeEventValues
	ref     legacyNativeCheckpointRef
	binding ContentHash
	store   legacyNativeCheckpointIO
	budget  int64
	state   legacyCanonicalReplacementState
	reader  *legacyNativeCheckpointListReader[uint64]
	seen    legacyCanonicalPages[uint64]
	result  legacyCanonicalReplacementRestored
	loaded  bool
	ready   bool
	closed  bool
	failure error
}

func newLegacyCanonicalReplacementRestore(ctx context.Context, source *legacyCanonicalReplacement, ref legacyNativeCheckpointRef, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalReplacementRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || source.native == nil || source.events != nil || source.length() < 0 || !ref.valid() ||
		ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	work := &legacyCanonicalReplacementRestore{source: source, native: source.native, ref: ref, binding: binding, store: store, budget: budget}
	if err := work.admit(0); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalReplacementRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)+len(work.state.Binding)+len(work.state.Entries.Ref.Hash)), 1)
	heap.include(work.result.projection)
	work.seen.retained(&heap)
	work.result.fields.retained(&heap)
	if work.reader != nil {
		retained, err := work.reader.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalReplacementRestore) admit(extra int64) error {
	retained, err := work.retainedBytes()
	if err != nil || extra < 0 || retained > work.budget || legacyNativeAnyCheckpointScratch > work.budget-retained ||
		extra > work.budget-retained-legacyNativeAnyCheckpointScratch {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyCanonicalReplacementRestore) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil || work.closed || work.source == nil || work.source.native != work.native || work.source.events != nil ||
		work.source.length() < 0 || work.loaded && work.source.length() != work.state.Order.Count {
		return ErrConflict
	}
	return work.admit(0)
}

func (work *legacyCanonicalReplacementRestore) step(ctx context.Context) (ready bool, failure error) {
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
	if work.ready {
		return true, nil
	}
	if !work.loaded {
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return false, err
		}
		state := work.state
		if state.Kind != "replacement-order" || state.Version != legacyNativeCheckpointVersion || state.Binding != work.binding ||
			state.Order.Count != work.source.length() || state.Order.Count < 0 || !state.Entries.valid() || state.Entries.Count != uint64(state.Order.Count) ||
			state.Order.CompareOffset < 0 || state.Order.CompareOffset >= 32 || state.Order.Count >= 2 && state.Order.Phase == legacyCanonicalKeyOrderDone {
			return false, ErrIntegrity
		}
		reader, err := newLegacyNativeCheckpointListReader[uint64](state.Entries, work.store)
		if err != nil {
			return false, err
		}
		work.reader, work.loaded = reader, true
		work.result.order = state.Order.value()
		return false, nil
	}
	words := work.state.Order.Count / 64
	if work.state.Order.Count%64 != 0 {
		words++
	}
	if work.seen.length() < words {
		_, peak, err := legacyNativeEventValuesAppendAllocation(&work.seen)
		if err != nil {
			return false, err
		}
		if err := work.admit(peak); err != nil {
			return false, err
		}
		return false, work.seen.append(0)
	}
	if work.result.fields.length() < work.state.Order.Count {
		_, peak, err := legacyNativeEventValuesAppendAllocation(&work.result.fields)
		if err != nil {
			return false, err
		}
		if err := work.admit(peak + legacyCanonicalReplacementProjectionBytes); err != nil {
			return false, err
		}
	}
	ordinal, found, err := work.reader.next(ctx)
	if err != nil {
		return false, err
	}
	if !found {
		if !legacyCanonicalReplacementOrderValid(work.result.order, &work.result.fields) {
			return false, ErrIntegrity
		}
		work.ready = true
		return true, nil
	}
	if ordinal >= uint64(work.state.Order.Count) || work.result.fields.length() >= work.state.Order.Count {
		return false, ErrIntegrity
	}
	seen := work.seen.at(int(ordinal / 64))
	mask := uint64(1) << (ordinal % 64)
	if seen == nil || *seen&mask != 0 {
		return false, ErrIntegrity
	}
	event, replacement, err := work.source.at(int(ordinal))
	if err != nil {
		return false, err
	}
	value, err := legacyCanonicalNativeEventViewJSONObject(event, replacement, work.native.at(int(ordinal)).blocks)
	if err != nil {
		return false, err
	}
	projection := legacyCanonicalNativeProjectionHeap(event, value)
	projection.allocation(32, 1)
	if projection.err != nil || projection.bytes > legacyCanonicalReplacementProjectionBytes {
		return false, ErrIntegrity
	}
	if err := work.result.fields.append(legacyCanonicalKeyField{key: legacyCanonicalReplacementKey(event.Seq, int(ordinal)), value: value}); err != nil {
		return false, err
	}
	work.result.projection.include(projection)
	*seen |= mask
	return false, work.result.projection.err
}

func (work *legacyCanonicalReplacementRestore) take(ctx context.Context) (legacyCanonicalReplacementRestored, error) {
	if err := work.current(ctx); err != nil {
		return legacyCanonicalReplacementRestored{}, err
	}
	if !work.ready {
		return legacyCanonicalReplacementRestored{}, ErrDocRangeIndexPending
	}
	result := work.result
	work.result = legacyCanonicalReplacementRestored{}
	work.close()
	return result, nil
}

func (work *legacyCanonicalReplacementRestore) close() {
	if work != nil && !work.closed {
		work.reader.close()
		work.seen.clear()
		work.result.fields.clear()
		*work = legacyCanonicalReplacementRestore{closed: true, failure: work.failure}
	}
}
