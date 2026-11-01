package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalPreparedNumbersRestore struct {
	state     legacyCanonicalPreparedNumberState
	binding   ContentHash
	store     legacyNativeCheckpointIO
	rows      *legacyCanonicalPreparedListReader[legacyCanonicalPreparedNumberRow]
	scalar    *legacyNativeCheckpointScalarRestore
	value     *legacyNativeScalar
	row       legacyCanonicalPreparedNumberRow
	texts     legacyCanonicalSourceNumberTexts
	keepTexts bool
	result    legacyCanonicalNumberValidation
	payload   docRangeHeaderHeapEstimate
	count     int
	position  int
	budget    int64
	header    bool
	ready     bool
	closed    bool
	failure   error
}

func newLegacyCanonicalPreparedNumbersRestore(ctx context.Context, state legacyCanonicalPreparedNumberState, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedNumbersRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !state.Pending.valid() || state.Pending.Count == 0 || state.Pending.Count-1 > uint64(int(^uint(0)>>1)) ||
		len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	count := int(state.Pending.Count - 1)
	if err := legacyCanonicalPreparedNumberCursor(state.Next, state.Position, state.Mode, count); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedNumbersRestore{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedListReader[legacyCanonicalPreparedNumberRow]{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeCursor{})))
	planned.allocation(int64(len(binding)+2*len(state.Pending.Ref.Hash)), 1)
	planned.include(legacyCanonicalPreparedNumbersWorkspace())
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, budget); err != nil {
		return nil, err
	}
	rows, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedNumberRow](state.Pending, store)
	if err != nil {
		return nil, err
	}
	return &legacyCanonicalPreparedNumbersRestore{
		state: state, binding: binding, store: store, rows: rows, count: count, budget: budget,
	}, nil
}

func (work *legacyCanonicalPreparedNumbersRestore) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	if work.value != nil {
		if work.keepTexts && work.texts.values.length() == work.position {
			if work.value.text == nil {
				return work.fail(ErrIntegrity)
			}
			if err := work.texts.append(work.row.Text, work.value.text.Cap(), work.checkBudget); err != nil {
				return work.fail(err)
			}
			return work.progress(ctx)
		}
		if err := work.appendValue(); err != nil {
			return work.fail(err)
		}
		return work.progress(ctx)
	}
	if work.scalar != nil {
		if err := work.checkBudget(legacyCanonicalPreparedNumbersWorkspace()); err != nil {
			return work.fail(err)
		}
		ready, err := work.scalar.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.value, err = work.scalar.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.scalar = nil
		}
		return work.progress(ctx)
	}
	if work.header && work.position == work.count {
		if work.result.pending.length() != work.count || work.result.proven.length() != work.count || (work.keepTexts && work.texts.values.length() != work.count) {
			return work.fail(ErrIntegrity)
		}
		work.result.next, work.result.position, work.result.mode = work.state.Next, work.state.Position, work.state.Mode
		work.rows.close()
		work.rows = nil
		work.ready = true
		return work.progress(ctx)
	}
	if err := work.checkBudget(legacyCanonicalPreparedNumbersWorkspace()); err != nil {
		return work.fail(err)
	}
	row, ok, err := work.rows.next(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ok {
		return work.progress(ctx)
	}
	if !work.header {
		if row != work.state.header(work.binding) {
			return work.fail(ErrIntegrity)
		}
		work.header = true
		return work.progress(ctx)
	}
	expected := legacyCanonicalPreparedNumberRow{
		Version: legacyNativeCheckpointVersion, Ordinal: work.position,
		Text: row.Text, Proven: work.position < work.state.Next,
	}
	if row != expected || work.position >= work.count ||
		work.position == work.state.Next && work.state.Position > row.Text.Bytes {
		return work.fail(ErrIntegrity)
	}
	capacity := max(1, row.Text.Bytes)
	if err := (legacyNativeCheckpointStringValueRow{Capacity: capacity, Text: row.Text}).validate(); err != nil {
		return work.fail(err)
	}
	work.row = row
	retained, err := work.retainedBytes()
	if err != nil {
		return work.fail(err)
	}
	scratch := legacyCanonicalPreparedNumbersWorkspace()
	if err := legacyNativeCheckpointStringValuesBudget(retained, scratch, work.budget); err != nil {
		return work.fail(err)
	}
	remaining := work.budget - retained - scratch.bytes
	if remaining <= 0 {
		return work.fail(ErrDocumentResourceLimit)
	}
	saved := legacyNativeCheckpointScalar{
		State: legacyNativeScalarCheckpoint{
			Version: 1, Maximum: int(legacyNativeValueCheckpointPayloadLimit()),
			Bytes: row.Text.Bytes, Capacity: capacity, Present: true,
		},
		Text: row.Text,
	}
	work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, saved, work.store, MaxFinalizedDocBytes, remaining)
	if err != nil {
		return work.fail(err)
	}
	return work.progress(ctx)
}

func (work *legacyCanonicalPreparedNumbersRestore) appendValue() error {
	value := work.value
	if value.text == nil || value.previous != nil || value.text.Len() != work.row.Text.Bytes ||
		value.position != 0 || value.end != 0 || value.buffered != 0 || value.expanded {
		return ErrIntegrity
	}
	text := value.text.String()
	identity := legacyCanonicalNumberKey(text)
	registryWork, exists, err := work.result.proven.plan(identity)
	if err != nil {
		return err
	}
	if exists {
		return ErrIntegrity
	}
	pendingWork, future, err := work.result.pending.appendRequirements()
	if err != nil {
		return err
	}
	proofWork := 0
	if work.row.Proven {
		proofWork = legacyCanonicalNumberProofBytes
	}
	if registryWork > LegacyCanonicalEncodingBytes-pendingWork-proofWork {
		return ErrIntegrity
	}
	var current, planned docRangeHeaderHeapEstimate
	work.result.pending.retained(&current)
	if current.err != nil || future.err != nil || future.bytes < current.bytes {
		return ErrIntegrity
	}
	planned.include(docRangeHeaderHeapEstimate{bytes: future.bytes - current.bytes})
	planned.include(legacyNativeCheckpointGoGraphPageRetirement(&work.result.pending, future))
	nodeBytes := int64(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{}))
	legacyCanonicalStorageAllocation(&planned, registryWork/int(nodeBytes), nodeBytes)
	if err := work.checkBudget(planned); err != nil {
		return err
	}
	if err := work.result.pending.append(text); err != nil {
		return err
	}
	work.result.proven.insertIndexed(identity, work.position)
	if work.row.Proven {
		if err := work.result.proven.prove(identity); err != nil {
			return err
		}
	}
	work.payload.allocation(int64(value.text.Cap()), 1)
	if work.payload.err != nil {
		return work.payload.err
	}
	work.value, work.row = nil, legacyCanonicalPreparedNumberRow{}
	work.position++
	return nil
}

func (work *legacyCanonicalPreparedNumbersRestore) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(docRangeHeaderHeapEstimate{}); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyCanonicalPreparedNumbersRestore) checkBudget(additional docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointStringValuesBudget(retained, additional, work.budget)
}

func (work *legacyCanonicalPreparedNumbersRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.binding)+len(work.state.Pending.Ref.Hash)), 1)
	heap.allocation(int64(len(work.row.Text.Tree.Ref.Hash)), 1)
	work.texts.retained(&heap)
	heap.include(work.payload)
	work.result.retained(&heap)
	if work.rows != nil {
		retained, err := work.rows.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.value != nil {
		work.value.retained(&heap)
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedNumbersRestore) take(ctx context.Context) (legacyCanonicalNumberValidation, docRangeHeaderHeapEstimate, error) {
	if work == nil {
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	result, heap := work.result, work.payload
	result.retained(&heap)
	if heap.err != nil {
		work.fail(heap.err)
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, heap.err
	}
	work.close()
	return result, heap, nil
}

func (work *legacyCanonicalPreparedNumbersRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalPreparedNumbersRestore) close() {
	if work != nil {
		if work.rows != nil {
			work.rows.close()
		}
		work.scalar.close()
		failure := work.failure
		*work = legacyCanonicalPreparedNumbersRestore{closed: true, failure: failure}
	}
}
