package domain

import (
	"context"
	"unsafe"
)

const (
	legacyNativeEventValuesEntryList uint8 = iota + 1
	legacyNativeEventValuesEntryRecord
	legacyNativeEventValuesEntryBlocks
	legacyNativeEventValuesEntryBlock
)

type legacyNativeEventValuesCheckpointEntry struct {
	Kind   uint8                             `json:"kind"`
	Nil    bool                              `json:"nil"`
	Count  int                               `json:"count"`
	Length int                               `json:"length"`
	Fields legacyNativeCheckpointRef         `json:"fields"`
	Block  legacyNativeCheckpointBlockRecord `json:"block"`
}

func (entry legacyNativeEventValuesCheckpointEntry) validate(kind uint8, sourceBytes int) error {
	if entry.Kind != kind || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes {
		return ErrIntegrity
	}
	switch kind {
	case legacyNativeEventValuesEntryList, legacyNativeEventValuesEntryBlocks:
		if entry.Fields != (legacyNativeCheckpointRef{}) || entry.Block != (legacyNativeCheckpointBlockRecord{}) || entry.Count < 0 || entry.Length < entry.Count || entry.Length > sourceBytes || entry.Nil && entry.Length != 0 {
			return ErrIntegrity
		}
	case legacyNativeEventValuesEntryRecord:
		if entry.Nil || entry.Count != 0 || entry.Length != 0 || entry.Block != (legacyNativeCheckpointBlockRecord{}) || !entry.Fields.valid() {
			return ErrIntegrity
		}
	case legacyNativeEventValuesEntryBlock:
		if entry.Nil || entry.Count != 0 || entry.Length != 0 || entry.Fields != (legacyNativeCheckpointRef{}) {
			return ErrIntegrity
		}
		schema, err := legacyNativeCheckpointBlockSchema()
		if err != nil || entry.Block.Schema != schema || entry.Block.Inline != nil || entry.Block.Values.Nil || entry.Block.Values.Length != 2 || entry.Block.Values.Capacity != 2 {
			return ErrIntegrity
		}
		return entry.Block.Values.validate(true)
	default:
		return ErrIntegrity
	}
	return nil
}

type legacyNativeEventValuesCheckpointRoot struct {
	Version     uint8                      `json:"v"`
	Schema      ContentHash                `json:"schema"`
	Binding     ContentHash                `json:"binding"`
	SourceBytes int                        `json:"source_bytes"`
	Entries     legacyNativeCheckpointTree `json:"entries"`
}

func (root legacyNativeEventValuesCheckpointRoot) validate(binding ContentHash, sourceBytes int) error {
	schema, err := legacyNativeEventRecordFieldSchema()
	if err != nil || root.Version != 1 || root.Schema != schema || root.Binding != binding || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || root.SourceBytes != sourceBytes || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes ||
		!root.Entries.valid() || root.Entries.Count == 0 || root.Entries.Count > 8*uint64(sourceBytes)+1 {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeEventValuesCheckpointFrame struct {
	parent     *legacyNativeEventValuesCheckpointFrame
	source     *legacyNativeEventValues
	record     legacyNativeEventValue
	fields     legacyNativeCheckpointRef
	recordRow  uint64
	payload    docRangeHeaderHeapEstimate
	index      int
	blockIndex int
	phase      uint8
}

func legacyNativeEventValuesCheckpointFrameHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValuesCheckpointFrame{})))
	heap.allocation(int64(4*(len(contentHashPrefix)+64)), 1)
	return heap
}

type legacyNativeEventValuesCheckpoint struct {
	source     *legacyNativeEventValues
	sourceHeap docRangeHeaderHeapEstimate
	current    *legacyNativeEventValuesCheckpointFrame
	capture    *legacyNormalizationCapture
	store      legacyNativeCheckpointIO
	state      legacyNativeEventValuesCheckpointRoot
	rows       legacyNativeCheckpointListWriter[legacyNativeEventValuesCheckpointEntry]
	fields     *legacyNativeEventRecordFieldsCheckpoint
	block      *legacyNativeCheckpointBlockRecordWriter
	root       legacyNativeCheckpointRef
	frames     int
	budget     int64
	finished   bool
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeEventValuesCheckpoint(ctx context.Context, list *legacyNativeEventList, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, budget int64) (*legacyNativeEventValuesCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if list == nil || list.mutation == nil || store.Put == nil || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeEventValuesCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	planned.include(legacyNativeEventValuesCheckpointFrameHeap())
	planned.include(docRangeHeaderHeapEstimate{bytes: 4 * LegacyCanonicalEncodingBytes})
	if planned.err != nil || planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	capture, err := list.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	if !list.paged || list.end < 0 || list.end > sourceBytes {
		capture.release()
		return nil, ErrIntegrity
	}
	heap := list.values.heap()
	planned.include(heap)
	if planned.err != nil || planned.bytes > budget {
		capture.release()
		if planned.err != nil {
			return nil, planned.err
		}
		return nil, ErrDocumentResourceLimit
	}
	schema, err := legacyNativeEventRecordFieldSchema()
	if err != nil {
		capture.release()
		return nil, err
	}
	work := &legacyNativeEventValuesCheckpoint{source: list.values, sourceHeap: heap, capture: capture, store: store, budget: budget, frames: 1,
		state:   legacyNativeEventValuesCheckpointRoot{Version: 1, Schema: schema, Binding: binding, SourceBytes: sourceBytes},
		current: &legacyNativeEventValuesCheckpointFrame{source: list.values}}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeEventValuesCheckpoint) checkSource() error {
	if work.capture == nil || !work.capture.valid(work.capture.scope) || work.source.heap() != work.sourceHeap {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeEventValuesCheckpoint) fieldCheckpoint(ctx context.Context, budget int64) (*legacyNativeEventRecordFieldsCheckpoint, error) {
	frame := work.current
	if err := work.checkSource(); err != nil {
		return nil, err
	}
	if frame == nil || frame.phase != 2 || frame.source == nil || frame.index < 0 || frame.index >= frame.source.records.length() {
		return nil, ErrIntegrity
	}
	record := frame.source.records.at(frame.index)
	if record == nil {
		return nil, ErrIntegrity
	}
	capture, err := work.capture.scope.beginCapture()
	if err != nil {
		return nil, err
	}
	child := &legacyNativeEventRecordFieldsCheckpoint{record: *record, capture: capture, store: work.store, budget: budget}
	if err := child.initialize(ctx, work.state.Binding); err != nil {
		child.close()
		return nil, err
	}
	return child, nil
}

func (work *legacyNativeEventValuesCheckpoint) pop() {
	work.current = work.current.parent
	work.frames--
}

func (work *legacyNativeEventValuesCheckpoint) advance(ctx context.Context) error {
	frame := work.current
	if frame == nil {
		if !work.finished {
			tree, err := work.rows.finish(ctx, work.store)
			if err != nil {
				return err
			}
			work.state.Entries, work.finished = tree, true
			work.rows.close()
			return nil
		}
		if err := work.state.validate(work.state.Binding, work.state.SourceBytes); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
		if err != nil {
			return err
		}
		work.root, work.ready = ref, true
		return nil
	}
	switch frame.phase {
	case 0:
		row := legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryList, Nil: frame.source == nil}
		if frame.source != nil {
			if frame.source.heap().err != nil {
				return ErrIntegrity
			}
			row.Count, row.Length = frame.source.count, frame.source.records.length()
		}
		if err := row.validate(row.Kind, work.state.SourceBytes); err != nil {
			return err
		}
		if err := work.rows.append(ctx, work.store, row); err != nil {
			return err
		}
		if frame.source == nil {
			work.pop()
		} else {
			frame.phase++
		}
	case 1:
		if frame.index == frame.source.records.length() {
			if frame.payload != frame.source.payload {
				return ErrIntegrity
			}
			work.pop()
			return nil
		}
		record := frame.source.records.at(frame.index)
		if record == nil || record.heap.err != nil || record.heap.bytes < 0 || record.heap.bytes > work.sourceHeap.bytes || record.excess < 0 || record.excess > record.heap.bytes {
			return ErrIntegrity
		}
		frame.recordRow = work.rows.tree.count + uint64(len(work.rows.values))
		frame.record, frame.phase = *record, 2
	case 2:
		if work.fields == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.fields, err = work.fieldCheckpoint(ctx, remaining+frame.record.heap.bytes)
			return err
		}
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return err
		}
		frame.fields, err = work.fields.take(ctx)
		work.fields = nil
		if err != nil {
			return err
		}
		frame.phase++
	case 3:
		if err := work.rows.append(ctx, work.store, legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryRecord, Fields: frame.fields}); err != nil {
			return err
		}
		frame.phase = 4
	case 4:
		blocks := frame.record.blocks
		row := legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryBlocks, Nil: blocks == nil}
		if blocks != nil {
			if blocks.heap().err != nil {
				return ErrIntegrity
			}
			row.Count, row.Length = blocks.count, blocks.records.length()
		}
		if err := row.validate(row.Kind, work.state.SourceBytes); err != nil {
			return err
		}
		if err := work.rows.append(ctx, work.store, row); err != nil {
			return err
		}
		frame.blockIndex, frame.phase = 0, 5
	case 5:
		blocks := frame.record.blocks
		if blocks == nil || frame.blockIndex == blocks.records.length() {
			frame.phase++
			return nil
		}
		if work.block == nil {
			record := blocks.records.at(frame.blockIndex)
			remaining, err := work.remaining()
			if err != nil || record == nil {
				if err != nil {
					return err
				}
				return ErrIntegrity
			}
			work.block, err = newLegacyNativeCheckpointBlockRecordWriter(ctx, *record, true, work.store, remaining)
			return err
		}
		ready, err := work.block.step(ctx)
		if err != nil || !ready {
			return err
		}
		record, err := work.block.take(ctx)
		work.block = nil
		if err != nil {
			return err
		}
		if err := work.rows.append(ctx, work.store, legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryBlock, Block: record}); err != nil {
			return err
		}
		frame.blockIndex++
	case 6:
		if work.frames >= 10000 {
			return ErrIntegrity
		}
		if err := work.admit(legacyNativeEventValuesCheckpointFrameHeap().bytes); err != nil {
			return err
		}
		frame.phase++
		work.current = &legacyNativeEventValuesCheckpointFrame{source: frame.record.replacement, parent: frame}
		work.frames++
	case 7:
		frame.payload.include(frame.record.heap)
		if frame.payload.err != nil {
			return frame.payload.err
		}
		frame.record = legacyNativeEventValue{}
		frame.fields, frame.recordRow = legacyNativeCheckpointRef{}, 0
		frame.index, frame.phase = frame.index+1, 1
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeEventValuesCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	heap.include(work.sourceHeap)
	if work.frames < 0 || work.frames > 10000 {
		return 0, ErrIntegrity
	}
	unit := legacyNativeEventValuesCheckpointFrameHeap()
	heap.include(docRangeHeaderHeapEstimate{bytes: int64(work.frames) * unit.bytes, err: unit.err})
	for _, hash := range []ContentHash{work.state.Schema, work.state.Binding, work.state.Entries.Ref.Hash, work.root.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	retained, err := work.rows.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.fields != nil {
		retained, err := work.fields.retainedBytes()
		if retained < work.fields.record.heap.bytes {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - work.fields.record.heap.bytes, err: err})
	}
	if work.block != nil {
		retained, err := work.block.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeEventValuesCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeEventValuesCheckpoint) admit(extra int64) error {
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

func (work *legacyNativeEventValuesCheckpoint) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeEventValuesCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	root := work.root
	work.close()
	return root, nil
}

func (work *legacyNativeEventValuesCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeEventValuesCheckpoint) close() {
	if work != nil && !work.closed {
		work.fields.close()
		work.block.close()
		work.rows.close()
		if work.capture != nil {
			work.capture.release()
		}
		failure := work.failure
		*work = legacyNativeEventValuesCheckpoint{closed: true, failure: failure}
	}
}
