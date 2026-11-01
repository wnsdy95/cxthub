package domain

import (
	"context"
	"unsafe"
)

type legacyNativeCheckpointBlockRecordWriter struct {
	values     [2]string
	capacities [2]int
	strings    *legacyNativeCheckpointStringValuesWriter
	result     legacyNativeCheckpointBlockRecord
	budget     int64
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeCheckpointBlockRecordWriter(ctx context.Context, record legacyNativeBlockValue, initialized bool, store legacyNativeCheckpointIO, heapBudget int64) (*legacyNativeCheckpointBlockRecordWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil {
		return nil, err
	}
	if store.Put == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	values := [2]string{record.block.Type, record.block.Text}
	var capacities [2]int
	maximum := legacyNativeValueCheckpointPayloadLimit()
	for ordinal, text := range values {
		if uint64(len(text)) > maximum {
			return nil, ErrIntegrity
		}
		capacity := int64(len(text))
		if initialized {
			excess := record.excess[ordinal]
			if record.heaps[ordinal].err != nil || record.heaps[ordinal].bytes < 0 || excess < 0 || excess%2 != 0 ||
				excess/2 > int64(2*maximum+64)-capacity {
				return nil, ErrIntegrity
			}
			capacity += excess / 2
			var expected docRangeHeaderHeapEstimate
			expected.allocation(capacity, 1)
			if expected.err != nil || record.heaps[ordinal].bytes != expected.bytes {
				return nil, ErrIntegrity
			}
		}
		if uint64(capacity) > uint64(int(^uint(0)>>1)) {
			return nil, ErrIntegrity
		}
		capacities[ordinal] = int(capacity)
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointBlockRecordWriter{})))
	planned.allocation(1, 64)
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, heapBudget); err != nil {
		return nil, err
	}
	if planned.bytes == heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeCheckpointBlockRecordWriter{values: values, capacities: capacities, budget: heapBudget}
	work.result.Schema = schema
	work.strings, err = newLegacyNativeCheckpointStringValuesWriter(ctx, work.values[:], work.capacity, store, heapBudget-planned.bytes)
	if err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointBlockRecordWriter) capacity(ctx context.Context, ordinal int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if work == nil || work.closed || ordinal < 0 || ordinal >= len(work.capacities) {
		return 0, ErrIntegrity
	}
	return work.capacities[ordinal], nil
}

func (work *legacyNativeCheckpointBlockRecordWriter) step(ctx context.Context) (bool, error) {
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
	if work.strings == nil {
		return work.fail(ErrIntegrity)
	}
	ready, err := work.strings.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ready && work.strings.pending && work.strings.text == nil && len(work.strings.values) == len(work.values) &&
		len(work.strings.rows.values) < legacyNativeCheckpointPageItems-1 {
		ready, err = work.strings.step(ctx)
		if err != nil {
			return work.fail(err)
		}
	}
	if ready {
		work.result.Values, err = work.strings.take(ctx)
		if err != nil {
			return work.fail(err)
		}
		if work.result.Values.Nil || work.result.Values.Length != 2 || work.result.Values.Capacity != 2 {
			return work.fail(ErrIntegrity)
		}
		work.strings = nil
		work.values, work.capacities = [2]string{}, [2]int{}
		work.ready = true
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	retained, err := work.retainedBytes()
	if err != nil {
		return work.fail(err)
	}
	if retained > work.budget {
		return work.fail(ErrDocumentResourceLimit)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointBlockRecordWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.result.Values.Items.Ref.Hash)), 1)
	heap.allocation(int64(len(work.result.Schema)), 1)
	if work.strings != nil {
		heap.allocation(1, 64)
		retained, err := work.strings.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointBlockRecordWriter) take(ctx context.Context) (legacyNativeCheckpointBlockRecord, error) {
	if work == nil {
		return legacyNativeCheckpointBlockRecord{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointBlockRecord{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointBlockRecord{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointBlockRecord{}, work.failure
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointBlockRecordWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointBlockRecordWriter) close() {
	if work != nil {
		work.strings.close()
		failure := work.failure
		*work = legacyNativeCheckpointBlockRecordWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointBlockRecordRestore struct {
	strings *legacyNativeCheckpointStringValuesRestore
	result  legacyNativeBlockValue
	budget  int64
	ready   bool
	closed  bool
	failure error
}

func newLegacyNativeCheckpointBlockRecordRestore(ctx context.Context, saved legacyNativeCheckpointBlockRecord, store legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointBlockRecordRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil || saved.Schema != schema {
		return nil, ErrIntegrity
	}
	if saved.Values.Nil || saved.Values.Length != 2 || saved.Values.Capacity != 2 ||
		store.Read == nil || sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	if err := saved.Values.validate(true); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointBlockRecordRestore{})))
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, heapBudget); err != nil {
		return nil, err
	}
	if planned.bytes == heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeCheckpointBlockRecordRestore{budget: heapBudget}
	work.strings, err = newLegacyNativeCheckpointStringValuesRestore(ctx, saved.Values, store, sourceBytes, heapBudget-planned.bytes)
	if err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointBlockRecordRestore) step(ctx context.Context) (bool, error) {
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
	if work.strings == nil {
		return work.fail(ErrIntegrity)
	}
	ready, err := work.strings.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if ready {
		current, err := work.strings.take(ctx)
		if err != nil {
			return work.fail(err)
		}
		work.strings = nil
		if current.Values == nil || len(current.Values) != 2 || cap(current.Values) != 2 ||
			len(current.Capacities) != 2 || cap(current.Capacities) != 2 || current.Payload.err != nil {
			return work.fail(ErrIntegrity)
		}
		var payload docRangeHeaderHeapEstimate
		for ordinal, text := range current.Values {
			capacity := current.Capacities[ordinal]
			if capacity < len(text) || uint64(capacity) > 2*legacyNativeValueCheckpointPayloadLimit()+64 {
				return work.fail(ErrIntegrity)
			}
			work.result.heaps[ordinal].allocation(int64(capacity), 1)
			work.result.excess[ordinal] = 2 * int64(capacity-len(text))
			payload.include(work.result.heaps[ordinal])
		}
		if payload.err != nil || payload.bytes != current.Payload.bytes {
			return work.fail(ErrIntegrity)
		}
		work.result.block = ContentBlock{Type: current.Values[0], Text: current.Values[1]}
		work.ready = true
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	retained, err := work.retainedBytes()
	if err != nil {
		return work.fail(err)
	}
	if retained > work.budget {
		return work.fail(ErrDocumentResourceLimit)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointBlockRecordRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	for _, payload := range work.result.heaps {
		heap.include(payload)
	}
	retained, err := work.strings.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointBlockRecordRestore) take(ctx context.Context) (legacyNativeBlockValue, error) {
	if work == nil {
		return legacyNativeBlockValue{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeBlockValue{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeBlockValue{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeBlockValue{}, work.failure
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointBlockRecordRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointBlockRecordRestore) close() {
	if work != nil {
		work.strings.close()
		failure := work.failure
		*work = legacyNativeCheckpointBlockRecordRestore{closed: true, failure: failure}
	}
}
