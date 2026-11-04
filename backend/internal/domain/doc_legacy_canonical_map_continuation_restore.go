package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalMapContinuationRestored struct {
	continuation *legacyCanonicalMapContinuation
}

type legacyCanonicalMapContinuationRestore struct {
	ref      legacyNativeCheckpointRef
	binding  ContentHash
	grant    *legacyNormalizationCapture
	scope    *legacyNormalizationMutation
	codec    legacyCanonicalMapContinuationValueCodec
	store    legacyNativeCheckpointIO
	state    legacyCanonicalMapContinuationState
	reader   *legacyCanonicalPreparedListReader[legacyCanonicalMapContinuationEntry]
	entry    legacyCanonicalMapContinuationEntry
	field    legacyCanonicalKeyField
	result   legacyCanonicalMapContinuationRestored
	budget   int64
	count    int
	position int
	phase    uint8
	ready    bool
	closed   bool
	failure  error
}

func newLegacyCanonicalMapContinuationRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, count, position int, grant *legacyNormalizationCapture, codec legacyCanonicalMapContinuationValueCodec, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalMapContinuationRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if grant == nil || !grant.valid(grant.scope) {
		return nil, ErrConflict
	}
	if !ref.valid() || ValidateContentHash(binding) != nil || position < 0 || count < position ||
		codec == nil || store.Read == nil {
		return nil, ErrIntegrity
	}
	work := &legacyCanonicalMapContinuationRestore{ref: ref, binding: binding, count: count, position: position,
		grant: grant, scope: grant.scope, codec: codec, store: store, budget: budget,
		result: legacyCanonicalMapContinuationRestored{continuation: &legacyCanonicalMapContinuation{}}}
	if err := work.current(ctx, 0); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalMapContinuationRestore) current(ctx context.Context, extra int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil || work.closed || !work.grant.valid(work.scope) {
		return ErrConflict
	}
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if err := legacyCanonicalMapContinuationAdmission(ctx, work.codec, retained, extra, work.budget); err != nil {
		return err
	}
	if !work.grant.valid(work.scope) {
		return ErrConflict
	}
	return nil
}

func (work *legacyCanonicalMapContinuationRestore) guardedStore() legacyNativeCheckpointIO {
	return legacyNativeCheckpointIO{Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		if err := work.current(ctx, 0); err != nil {
			return nil, err
		}
		body, err := work.store.Read(ctx, hash, length)
		if err != nil {
			return nil, err
		}
		if err := work.current(ctx, 0); err != nil {
			return nil, err
		}
		return body, nil
	}}
}

func (work *legacyCanonicalMapContinuationRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, 128)
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)+len(work.state.Kind)+len(work.state.Binding)+len(work.state.Entries.Ref.Hash)+len(work.entry.Key.Tree.Ref.Hash)+len(work.entry.Value.Hash)), 1)
	if work.result.continuation != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.result.continuation)))
		work.result.continuation.fields.retained(&heap)
	}
	retained, err := work.reader.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyCanonicalMapContinuationRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if err := legacyNativeCheckpointReadJSON(ctx, work.guardedStore(), work.ref, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.binding, work.count, work.position); err != nil {
			return err
		}
		reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalMapContinuationEntry](work.state.Entries, work.guardedStore())
		if err != nil {
			return err
		}
		work.reader, work.phase = reader, 1
	case 1:
		entry, found, err := work.reader.next(ctx)
		if err != nil {
			return err
		}
		if !found {
			if work.result.continuation.fields.length() != work.count-work.position {
				return ErrIntegrity
			}
			work.ready = true
			return nil
		}
		if work.result.continuation.fields.length() >= work.count-work.position {
			return ErrIntegrity
		}
		if err := entry.validate(); err != nil {
			return err
		}
		work.entry, work.phase = entry, 2
	case 2:
		if err := work.entry.validate(); err != nil {
			return err
		}
		key, err := work.codec.decodeKey(ctx, work.entry.Key)
		if err != nil {
			return err
		}
		if err := work.current(ctx, 0); err != nil {
			return err
		}
		if len(key) != work.entry.Key.Bytes {
			return ErrIntegrity
		}
		work.field.key, work.phase = key, 3
	case 3:
		value, err := work.codec.decode(ctx, work.entry.Value)
		if err != nil {
			return err
		}
		work.field.value, work.phase = value, 4
	case 4:
		_, peak, err := legacyNativeEventValuesAppendAllocation(&work.result.continuation.fields)
		if err != nil {
			return err
		}
		if err := work.current(ctx, peak); err != nil {
			return err
		}
		if err := work.result.continuation.fields.append(work.field); err != nil {
			return err
		}
		work.entry, work.field, work.phase = legacyCanonicalMapContinuationEntry{}, legacyCanonicalKeyField{}, 1
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyCanonicalMapContinuationRestore) step(ctx context.Context) (ready bool, failure error) {
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
	if err := work.current(ctx, 0); err != nil {
		return false, err
	}
	if !work.ready {
		if err := work.advance(ctx); err != nil {
			return false, err
		}
	}
	if err := work.current(ctx, 0); err != nil {
		return false, err
	}
	return work.ready, nil
}

func (work *legacyCanonicalMapContinuationRestore) take(ctx context.Context) (legacyCanonicalMapContinuationRestored, error) {
	if work == nil {
		return legacyCanonicalMapContinuationRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyCanonicalMapContinuationRestored{}, work.failure
	}
	if err := work.current(ctx, 0); err != nil {
		work.close()
		work.failure = err
		return legacyCanonicalMapContinuationRestored{}, err
	}
	if !work.ready {
		return legacyCanonicalMapContinuationRestored{}, ErrDocRangeIndexPending
	}
	result := work.result
	work.result = legacyCanonicalMapContinuationRestored{}
	work.close()
	return result, nil
}

func (work *legacyCanonicalMapContinuationRestore) close() {
	if work != nil && !work.closed {
		work.reader.close()
		*work = legacyCanonicalMapContinuationRestore{closed: true, failure: work.failure}
	}
}
