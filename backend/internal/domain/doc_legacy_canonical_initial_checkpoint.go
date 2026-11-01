package domain

import (
	"context"
	"unsafe"
)

const legacyCanonicalSourceCheckpointTransitions = 8

type legacyCanonicalSourceCheckpoint struct {
	encoding    *LegacyCanonicalEncoding
	owner       *legacyNativeCanonicalOwner
	preparation *legacyCanonicalPreparation
	list        *legacyNativeEventList
	child       *legacyNativeEventValuesCheckpoint
	store       legacyNativeCheckpointIO
	borrowed    docRangeHeaderHeapEstimate
	budget      int64
	initial     bool
	wrote       bool
	closed      bool
	failure     error
}

func legacyCanonicalSourceCheckpointMetadata() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalSourceCheckpoint{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventList{})))
	heap.allocation(2, int64(unsafe.Sizeof(uintptr(0))))
	return heap
}

func newLegacyCanonicalInitialCheckpoint(ctx context.Context, encoding *LegacyCanonicalEncoding, scope *legacyNormalizationMutation, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalSourceCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !legacyCanonicalInitialState(encoding) {
		return nil, ErrIntegrity
	}
	work, err := newLegacyCanonicalSourceCheckpoint(ctx, encoding, scope, binding, sourceBytes, store, budget)
	if err == nil {
		work.initial = true
	}
	return work, err
}

func legacyCanonicalSourceCheckpointState(encoding *LegacyCanonicalEncoding) bool {
	return encoding != nil && encoding.initialized && !encoding.complete && encoding.err == nil && encoding.nativeOwner != nil &&
		encoding.nativeOwner.seal == nil && encoding.preparation != nil && encoding.preparation.frames.valid() &&
		(encoding.preparation.frames.length() == 0) == encoding.preparation.complete && encoding.frames.length() == 0
}

func newLegacyCanonicalSourceCheckpoint(ctx context.Context, encoding *LegacyCanonicalEncoding, scope *legacyNormalizationMutation, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalSourceCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !legacyCanonicalSourceCheckpointState(encoding) || scope == nil || store.Put == nil || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes ||
		len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	if !scope.capture.Load().valid(scope) {
		return nil, ErrConflict
	}
	owner := encoding.nativeOwner
	if owner.heap.err != nil || owner.heap.bytes < 0 {
		return nil, ErrIntegrity
	}
	metadata := legacyCanonicalSourceCheckpointMetadata()
	var records legacyCanonicalPages[legacyNativeEventValue]
	_, backing, err := records.appendRequirements()
	if err != nil {
		return nil, err
	}
	planned := metadata
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValues{})))
	planned.include(backing)
	planned.include(docRangeHeaderHeapEstimate{bytes: 4 * LegacyCanonicalEncodingBytes})
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	childBudget := docRangeHeaderHeapEstimate{bytes: budget - metadata.bytes}
	childBudget.include(owner.heap)
	childBudget.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	if childBudget.err != nil {
		return nil, childBudget.err
	}
	values := &legacyNativeEventValues{payload: owner.heap, count: 1}
	if err := values.records.append(legacyNativeEventValue{event: owner.event, replacement: owner.replacement, blocks: owner.blocks, heap: owner.heap}); err != nil {
		return nil, err
	}
	work := &legacyCanonicalSourceCheckpoint{encoding: encoding, owner: owner, preparation: encoding.preparation, borrowed: owner.heap, budget: budget, store: store,
		list: &legacyNativeEventList{mutation: scope, values: values, paged: true, end: sourceBytes}}
	work.child, err = newLegacyNativeEventValuesCheckpoint(ctx, work.list, binding, sourceBytes, legacyNativeCheckpointIO{Put: work.put, Read: store.Read}, childBudget.bytes)
	if err != nil {
		work.close()
		return nil, err
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalSourceCheckpoint) checkSource() error {
	if !legacyCanonicalSourceCheckpointState(work.encoding) || work.initial && !legacyCanonicalInitialState(work.encoding) ||
		work.encoding.nativeOwner != work.owner || work.encoding.preparation != work.preparation || work.owner.heap != work.borrowed || work.list == nil || work.child == nil ||
		work.child.source != work.list.values || !work.child.capture.valid(work.list.mutation) {
		return ErrConflict
	}
	return nil
}

func (work *legacyCanonicalSourceCheckpoint) borrowedHeap(pending bool) docRangeHeaderHeapEstimate {
	heap := work.borrowed
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	if work.child.fields != nil {
		if work.child.fields.capture != work.child.capture {
			heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
			return heap
		}
		heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	} else if pending && work.child.current != nil && work.child.current.phase == 2 {
		heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	}
	return heap
}

func (work *legacyCanonicalSourceCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	if work.child == nil {
		return 0, ErrIntegrity
	}
	heap := legacyCanonicalSourceCheckpointMetadata()
	retained, err := work.child.retainedBytes()
	if err != nil {
		return 0, err
	}
	borrowed := work.borrowedHeap(false)
	if borrowed.err != nil || retained < borrowed.bytes {
		return 0, ErrIntegrity
	}
	heap.include(docRangeHeaderHeapEstimate{bytes: retained - borrowed.bytes})
	return heap.bytes, heap.err
}

func (work *legacyCanonicalSourceCheckpoint) admit(extra int64) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if retained > work.budget || extra > work.budget-retained {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyCanonicalSourceCheckpoint) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	work.wrote = false
	for transitions := 0; transitions < legacyCanonicalSourceCheckpointTransitions; transitions++ {
		ready, err := work.stepOnce(ctx)
		if err != nil || ready || work.wrote || work.sourceGraph() != nil || work.sourceRecord() != nil {
			return ready, err
		}
	}
	return false, nil
}

func (work *legacyCanonicalSourceCheckpoint) put(ctx context.Context, hash ContentHash, body []byte) error {
	work.wrote = true
	return work.store.Put(ctx, hash, body)
}

func (work *legacyCanonicalSourceCheckpoint) stepOnce(ctx context.Context) (bool, error) {
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
	if work.child.ready {
		return true, nil
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	metadata := legacyCanonicalSourceCheckpointMetadata()
	childBudget := docRangeHeaderHeapEstimate{bytes: work.budget - metadata.bytes}
	childBudget.include(work.borrowedHeap(true))
	if childBudget.err != nil {
		return work.fail(childBudget.err)
	}
	work.child.budget = childBudget.bytes
	ready, err := work.child.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return ready, nil
}

func (work *legacyCanonicalSourceCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	ref, err := work.child.take(ctx)
	if err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	work.close()
	return ref, nil
}

func (work *legacyCanonicalSourceCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalSourceCheckpoint) close() {
	if work != nil && !work.closed {
		work.child.close()
		failure := work.failure
		*work = legacyCanonicalSourceCheckpoint{closed: true, failure: failure}
	}
}

type legacyCanonicalInitialRestore struct {
	child      *legacyNativeEventValuesCheckpointRestore
	encoding   *LegacyCanonicalEncoding
	projection *legacyCanonicalSourceProjection
	store      legacyNativeCheckpointIO
	budget     int64
	read       bool
	closed     bool
	failure    error
}

func newLegacyCanonicalInitialRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalInitialRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var metadata docRangeHeaderHeapEstimate
	metadata.allocation(1, int64(unsafe.Sizeof(legacyCanonicalInitialRestore{})))
	metadata.allocation(2, int64(unsafe.Sizeof(uintptr(0))))
	if metadata.err != nil || metadata.bytes >= budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyCanonicalInitialRestore{store: store, budget: budget}
	child, err := newLegacyNativeEventValuesCheckpointRestore(ctx, ref, binding, sourceBytes, legacyNativeCheckpointIO{Put: store.Put, Read: work.readObject}, budget-metadata.bytes)
	if err != nil {
		return nil, err
	}
	work.child = child
	return work, nil
}

func (work *legacyCanonicalInitialRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(2, int64(unsafe.Sizeof(uintptr(0))))
	retained, err := work.child.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.encoding != nil {
		retained, err := work.encoding.RetainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.projection != nil {
		retained, err := work.projection.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalInitialRestore) admit(extra int64) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if retained > work.budget || extra > work.budget-retained {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyCanonicalInitialRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	work.read = false
	for transitions := 0; transitions < legacyCanonicalSourceCheckpointTransitions; transitions++ {
		ready, err := work.stepOnce(ctx)
		if err != nil || ready || work.read || work.sourceGraph() != nil || work.sourceRecord() != nil {
			return ready, err
		}
	}
	return false, nil
}

func (work *legacyCanonicalInitialRestore) readObject(ctx context.Context, hash ContentHash, limit int) ([]byte, error) {
	work.read = true
	return work.store.Read(ctx, hash, limit)
}

func (work *legacyCanonicalInitialRestore) stepOnce(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed || work.child == nil && work.encoding == nil {
		return false, ErrIntegrity
	}
	if work.encoding != nil {
		return true, nil
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	ready, err := work.child.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ready {
		return false, nil
	}
	result, err := work.child.take(ctx)
	work.child = nil
	if err != nil {
		return work.fail(err)
	}
	values := result.Values
	if values == nil || values.count != 1 || values.records.length() != 1 || !values.records.valid() {
		return work.fail(ErrIntegrity)
	}
	record := values.records.at(0)
	if record == nil || record.heap.err != nil || record.heap.bytes < 0 || values.payload != record.heap || result.Excess != record.excess || record.excess < 0 || record.excess > record.heap.bytes {
		return work.fail(ErrIntegrity)
	}
	if work.projection != nil {
		if work.projection.closed || work.projection.record.heap != record.heap || work.projection.record.blocks != record.blocks ||
			work.projection.record.replacement != record.replacement || work.projection.reference.Record != 1 {
			return work.fail(ErrIntegrity)
		}
		work.encoding, err = work.projection.takeEncoding(ctx)
		work.projection.close()
		work.projection = nil
	} else {
		owner := &legacyNativeCanonicalOwner{event: record.event, replacement: record.replacement, blocks: record.blocks, heap: record.heap}
		owner.seal = owner
		work.encoding, err = newLegacyCanonicalEncodingNative(ctx, owner)
		if err != nil {
			owner.release()
		}
	}
	*record = legacyNativeEventValue{}
	*values = legacyNativeEventValues{}
	result = legacyNativeEventValuesRestored{}
	if err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return true, nil
}

func (work *legacyCanonicalInitialRestore) take(ctx context.Context) (*LegacyCanonicalEncoding, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, err
	}
	if work.closed || work.encoding == nil {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	encoding := work.encoding
	work.encoding = nil
	work.close()
	return encoding, nil
}

func (work *legacyCanonicalInitialRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalInitialRestore) close() {
	if work != nil && !work.closed {
		work.child.close()
		work.projection.close()
		if work.encoding != nil {
			work.encoding.release()
		}
		failure := work.failure
		*work = legacyCanonicalInitialRestore{closed: true, failure: failure}
	}
}
