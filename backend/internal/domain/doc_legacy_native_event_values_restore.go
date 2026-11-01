package domain

import (
	"context"
	"unsafe"
)

type legacyNativeEventValuesRestored struct {
	Values *legacyNativeEventValues
	Excess int64
}

type legacyNativeEventValuesRestoreFrame struct {
	parent      *legacyNativeEventValuesRestoreFrame
	values      *legacyNativeEventValues
	header      legacyNativeEventValuesCheckpointEntry
	blocks      legacyNativeEventValuesCheckpointEntry
	record      legacyNativeEventValue
	fields      legacyNativeCheckpointRef
	recordRow   uint64
	excess      docRangeHeaderHeapEstimate
	blockExcess docRangeHeaderHeapEstimate
	index       int
	blockIndex  int
	phase       uint8
}

func legacyNativeEventValuesRestoreFrameHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValuesRestoreFrame{})))
	heap.allocation(int64(4*(len(contentHashPrefix)+64)), 1)
	return heap
}

func legacyNativeEventValuesAppendAllocation[Value any](pages *legacyCanonicalPages[Value]) (int64, int64, error) {
	_, future, err := pages.appendRequirements()
	if err != nil {
		return 0, 0, err
	}
	var current docRangeHeaderHeapEstimate
	pages.retained(&current)
	retired := legacyNativeCheckpointGoGraphPageRetirement(pages, future)
	if current.err != nil || future.err != nil || retired.err != nil || future.bytes < current.bytes {
		return 0, 0, ErrIntegrity
	}
	growth := future.bytes - current.bytes
	peak := docRangeHeaderHeapEstimate{bytes: growth}
	peak.include(retired)
	return growth, peak.bytes, peak.err
}

type legacyNativeEventValuesCheckpointRestore struct {
	ref         legacyNativeCheckpointRef
	binding     ContentHash
	sourceBytes int
	store       legacyNativeCheckpointIO
	state       legacyNativeEventValuesCheckpointRoot
	rows        *legacyNativeCheckpointListReader[legacyNativeEventValuesCheckpointEntry]
	current     *legacyNativeEventValuesRestoreFrame
	fields      *legacyNativeEventRecordFieldsRestore
	block       *legacyNativeCheckpointBlockRecordRestore
	decoded     docRangeHeaderHeapEstimate
	result      legacyNativeEventValuesRestored
	frames      int
	budget      int64
	loaded      bool
	ready       bool
	closed      bool
	failure     error
}

func newLegacyNativeEventValuesCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, budget int64) (*legacyNativeEventValuesCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValuesCheckpointRestore{})))
	planned.allocation(int64(len(ref.Hash)+len(binding)), 1)
	planned.include(legacyNativeEventValuesRestoreFrameHeap())
	planned.include(docRangeHeaderHeapEstimate{bytes: 4 * LegacyCanonicalEncodingBytes})
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	return &legacyNativeEventValuesCheckpointRestore{ref: ref, binding: binding, sourceBytes: sourceBytes, store: store, budget: budget}, nil
}

func (work *legacyNativeEventValuesCheckpointRestore) read(ctx context.Context, kind uint8) (legacyNativeEventValuesCheckpointEntry, error) {
	entry, ok, err := work.rows.next(ctx)
	if err != nil {
		return entry, err
	}
	if !ok {
		return entry, ErrIntegrity
	}
	return entry, entry.validate(kind, work.sourceBytes)
}

func (work *legacyNativeEventValuesCheckpointRestore) finishFrame() error {
	frame := work.current
	if frame == nil || frame.index != frame.header.Length {
		return ErrIntegrity
	}
	if frame.values != nil {
		if frame.values.records.length() != frame.header.Length {
			return ErrIntegrity
		}
		frame.values.count = frame.header.Count
		if heap := frame.values.heap(); heap.err != nil || frame.excess.err != nil || frame.excess.bytes > heap.bytes {
			return ErrIntegrity
		}
	}
	result := legacyNativeEventValuesRestored{Values: frame.values, Excess: frame.excess.bytes}
	work.current, work.frames = frame.parent, work.frames-1
	if work.current == nil {
		work.result = result
		return nil
	}
	parent := work.current
	if parent.phase != 6 {
		return ErrIntegrity
	}
	parent.record.replacement = result.Values
	parent.record.heap.include(result.Values.heap())
	extra := docRangeHeaderHeapEstimate{bytes: parent.record.excess}
	extra.include(docRangeHeaderHeapEstimate{bytes: result.Excess})
	if parent.record.heap.err != nil || extra.err != nil {
		return ErrIntegrity
	}
	parent.record.excess, parent.phase = extra.bytes, 7
	return nil
}

func (work *legacyNativeEventValuesCheckpointRestore) appendBlock(ctx context.Context) error {
	frame := work.current
	blocks := frame.record.blocks
	if blocks == nil {
		return ErrIntegrity
	}
	record, err := work.block.take(ctx)
	work.block = nil
	if err != nil {
		return err
	}
	var payload, excess docRangeHeaderHeapEstimate
	for index, heap := range record.heaps {
		payload.include(heap)
		excess.include(docRangeHeaderHeapEstimate{bytes: record.excess[index]})
	}
	if payload.err != nil || excess.err != nil || excess.bytes > payload.bytes {
		return ErrIntegrity
	}
	work.decoded.include(payload)
	blocks.payload.include(payload)
	frame.blockExcess.include(excess)
	if work.decoded.err != nil || blocks.payload.err != nil || frame.blockExcess.err != nil {
		return ErrIntegrity
	}
	growth, peak, err := legacyNativeEventValuesAppendAllocation(&blocks.records)
	if err != nil {
		return err
	}
	if err := work.admit(peak); err != nil {
		return err
	}
	if err := blocks.records.append(record); err != nil {
		return err
	}
	work.decoded.include(docRangeHeaderHeapEstimate{bytes: growth})
	frame.blockIndex++
	return work.decoded.err
}

func (work *legacyNativeEventValuesCheckpointRestore) advance(ctx context.Context) error {
	if !work.loaded {
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.binding, work.sourceBytes); err != nil {
			return err
		}
		var err error
		work.rows, err = newLegacyNativeCheckpointListReader[legacyNativeEventValuesCheckpointEntry](work.state.Entries, work.store)
		if err != nil {
			return err
		}
		if err := work.admit(legacyNativeEventValuesRestoreFrameHeap().bytes); err != nil {
			return err
		}
		work.current = &legacyNativeEventValuesRestoreFrame{}
		work.frames, work.loaded = 1, true
		return nil
	}
	frame := work.current
	if frame == nil {
		_, ok, err := work.rows.next(ctx)
		if err != nil {
			return err
		}
		if ok || work.result.Values.heap() != work.decoded {
			return ErrIntegrity
		}
		work.rows.close()
		work.rows = nil
		work.ready = true
		return nil
	}
	switch frame.phase {
	case 0:
		header, err := work.read(ctx, legacyNativeEventValuesEntryList)
		if err != nil {
			return err
		}
		frame.header = header
		if header.Nil {
			return work.finishFrame()
		}
		var heap docRangeHeaderHeapEstimate
		heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValues{})))
		if err := work.admit(heap.bytes); err != nil {
			return err
		}
		frame.values = &legacyNativeEventValues{}
		work.decoded.include(heap)
		frame.phase++
	case 1:
		if frame.index == frame.header.Length {
			return work.finishFrame()
		}
		entry, err := work.read(ctx, legacyNativeEventValuesEntryRecord)
		if err != nil {
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		work.fields, err = newLegacyNativeEventRecordFieldsRestore(ctx, entry.Fields, work.binding, work.store, remaining)
		if err != nil {
			return err
		}
		frame.fields = entry.Fields
		frame.recordRow = work.rows.cursor.read - uint64(len(work.rows.values)) + uint64(work.rows.index-1)
		frame.phase++
	case 2:
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return err
		}
		result, err := work.fields.take(ctx)
		work.fields = nil
		if err != nil {
			return err
		}
		if result.Heap.err != nil || result.Heap.bytes < 0 || result.Excess < 0 || result.Excess > result.Heap.bytes {
			return ErrIntegrity
		}
		frame.record = legacyNativeEventValue{event: result.Event, heap: result.Heap, excess: result.Excess}
		work.decoded.include(result.Heap)
		if work.decoded.err != nil {
			return work.decoded.err
		}
		frame.phase++
	case 3:
		header, err := work.read(ctx, legacyNativeEventValuesEntryBlocks)
		if err != nil {
			return err
		}
		frame.blocks = header
		if !header.Nil {
			var heap docRangeHeaderHeapEstimate
			heap.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockValues{})))
			if err := work.admit(heap.bytes); err != nil {
				return err
			}
			frame.record.blocks = &legacyNativeBlockValues{}
			work.decoded.include(heap)
		}
		frame.phase++
	case 4:
		blocks := frame.record.blocks
		if blocks == nil || frame.blockIndex == frame.blocks.Length {
			if blocks != nil {
				blocks.count = frame.blocks.Count
				frame.record.heap.include(blocks.heap())
				extra := docRangeHeaderHeapEstimate{bytes: frame.record.excess}
				extra.include(frame.blockExcess)
				if frame.record.heap.err != nil || extra.err != nil {
					return ErrIntegrity
				}
				frame.record.excess = extra.bytes
			}
			frame.phase = 5
			return nil
		}
		if work.block == nil {
			entry, err := work.read(ctx, legacyNativeEventValuesEntryBlock)
			if err != nil {
				return err
			}
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.block, err = newLegacyNativeCheckpointBlockRecordRestore(ctx, entry.Block, work.store, work.sourceBytes, remaining)
			return err
		}
		ready, err := work.block.step(ctx)
		if err != nil || !ready {
			return err
		}
		return work.appendBlock(ctx)
	case 5:
		if work.frames >= 10000 {
			return ErrIntegrity
		}
		if err := work.admit(legacyNativeEventValuesRestoreFrameHeap().bytes); err != nil {
			return err
		}
		frame.phase = 6
		work.current = &legacyNativeEventValuesRestoreFrame{parent: frame}
		work.frames++
	case 6:
		return ErrIntegrity
	case 7:
		growth, peak, err := legacyNativeEventValuesAppendAllocation(&frame.values.records)
		if err != nil {
			return err
		}
		if err := work.admit(peak); err != nil {
			return err
		}
		if err := frame.values.records.append(frame.record); err != nil {
			return err
		}
		work.decoded.include(docRangeHeaderHeapEstimate{bytes: growth})
		frame.values.payload.include(frame.record.heap)
		frame.excess.include(docRangeHeaderHeapEstimate{bytes: frame.record.excess})
		if work.decoded.err != nil || frame.values.payload.err != nil || frame.excess.err != nil {
			return ErrIntegrity
		}
		frame.record, frame.blockExcess = legacyNativeEventValue{}, docRangeHeaderHeapEstimate{}
		frame.fields, frame.recordRow = legacyNativeCheckpointRef{}, 0
		frame.index, frame.blockIndex, frame.phase = frame.index+1, 0, 1
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventValuesCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.decoded)
	if work.frames < 0 || work.frames > 10000 {
		return 0, ErrIntegrity
	}
	unit := legacyNativeEventValuesRestoreFrameHeap()
	heap.include(docRangeHeaderHeapEstimate{bytes: int64(work.frames) * unit.bytes, err: unit.err})
	for _, hash := range []ContentHash{work.ref.Hash, work.binding, work.state.Schema, work.state.Binding, work.state.Entries.Ref.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	for _, task := range []interface{ retainedBytes() (int64, error) }{work.rows, work.fields, work.block} {
		retained, err := task.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventValuesCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeEventValuesCheckpointRestore) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeEventValuesCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeEventValuesCheckpointRestore) take(ctx context.Context) (legacyNativeEventValuesRestored, error) {
	if work == nil {
		return legacyNativeEventValuesRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeEventValuesRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeEventValuesRestored{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeEventValuesRestored{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeEventValuesCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventValuesCheckpointRestore) close() {
	if work != nil && !work.closed {
		work.rows.close()
		work.fields.close()
		work.block.close()
		failure := work.failure
		*work = legacyNativeEventValuesCheckpointRestore{closed: true, failure: failure}
	}
}
