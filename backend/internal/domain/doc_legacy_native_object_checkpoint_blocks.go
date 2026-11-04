package domain

import (
	"context"
	"reflect"
	"strings"
	"unsafe"
)

func (state legacyNativeObjectCheckpointBlockField) validate(sourceBytes int) error {
	if sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || state.Binding > 3 || !state.Initialized && !state.Destination.Nil {
		return ErrIntegrity
	}
	if err := state.Destination.validate(state.Binding == 0); err != nil {
		return err
	}
	if state.Parser == (legacyNativeCheckpointRef{}) {
		if state.Binding != 0 || state.Paged && !state.Destination.Nil {
			return ErrIntegrity
		}
		return nil
	}
	if !legacyNativeCheckpointBlockListHashValid(state.Parser.Hash) || !state.Parser.valid() || state.Parser.Bytes > 16384 {
		return ErrIntegrity
	}
	switch state.Binding {
	case 0:
		if state.Paged {
			return ErrIntegrity
		}
	case 1, 2:
		if state.Paged || state.Destination.Nil || state.Destination.Capacity == 0 {
			return ErrIntegrity
		}
	case 3:
		if !state.Paged || !state.Destination.Nil || state.Destination.Length != 0 || state.Destination.Capacity != 0 {
			return ErrIntegrity
		}
	}
	return nil
}

func legacyNativeObjectCheckpointBlocksShape(values []ContentBlock) legacyNativeCheckpointBlockArray {
	return legacyNativeCheckpointBlockArray{Nil: values == nil, Length: len(values), Capacity: cap(values)}
}

func legacyNativeObjectCheckpointBlocksBinding(values []ContentBlock, parser *legacyNativeBlockList) uint8 {
	if parser == nil {
		return 0
	}
	if parser.paged {
		return 3
	}
	if cap(values) != 0 {
		if cap(values) == cap(parser.decoded) && unsafe.SliceData(values) == unsafe.SliceData(parser.decoded) {
			return 1
		}
		if cap(values) == cap(parser.previous) && unsafe.SliceData(values) == unsafe.SliceData(parser.previous) {
			return 2
		}
	}
	return 0
}

type legacyNativeObjectBlockCheckpointSnapshot struct {
	metadata          *legacyNativeObjectFields
	destination       uintptr
	data              *ContentBlock
	shape             legacyNativeCheckpointBlockArray
	heap              docRangeHeaderHeapEstimate
	excess            int64
	parser            *legacyNativeBlockList
	parserShape       legacyNativeCheckpointBlockListState
	frame             *legacyJSONValueScanner
	checkpointScanner *legacyJSONValueScanner
	parentDepth       int
	position          int
	end               int
	valueStart        int
	valueEnd          int
	field             int
	phase             uint8
	first             byte
	paged             bool
	active            bool
	skip              bool
}

func legacyNativeObjectBlockCheckpointSnapshotFor(native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, sourceBytes int) (legacyNativeObjectBlockCheckpointSnapshot, []ContentBlock, error) {
	var snapshot legacyNativeObjectBlockCheckpointSnapshot
	if native == nil || native.metadata == nil || native.metadata.err != nil || len(native.metadata.fields) > 32 ||
		field < 0 || field >= len(native.metadata.fields) || !native.metadata.fields[field].blockList ||
		len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) ||
		native.blocks != nil && len(native.blocks) != len(native.metadata.fields) ||
		!native.destination.IsValid() || native.destination.Kind() != reflect.Pointer || native.destination.IsNil() ||
		native.destination.Elem().Type() != native.metadata.model || scanner == nil || scanner.invalid {
		return snapshot, nil, ErrIntegrity
	}
	metadata := native.metadata.fields[field]
	if metadata.index < 0 || metadata.index >= native.destination.Elem().NumField() {
		return snapshot, nil, ErrIntegrity
	}
	values, valid := native.destination.Elem().Field(metadata.index).Interface().([]ContentBlock)
	if !valid {
		return snapshot, nil, ErrIntegrity
	}
	snapshot = legacyNativeObjectBlockCheckpointSnapshot{
		metadata: native.metadata, destination: native.destination.Pointer(), data: unsafe.SliceData(values),
		shape: legacyNativeObjectCheckpointBlocksShape(values), heap: native.heaps[field], excess: native.backingExcess[field],
		checkpointScanner: native.checkpointScanner, parentDepth: native.parentDepth, position: native.position, end: native.end,
		valueStart: native.valueStart, valueEnd: native.valueEnd, field: native.field, phase: native.phase,
		first: native.valueFirst, paged: native.pagedEvents, active: native.blockActive, skip: native.skipValue,
	}
	if err := snapshot.shape.validate(false); err != nil {
		return snapshot, nil, err
	}
	if snapshot.heap.err != nil || snapshot.heap.bytes < 0 || snapshot.excess < 0 || snapshot.excess%2 != 0 ||
		snapshot.heap.bytes == 0 && snapshot.excess != 0 || snapshot.paged && values != nil {
		return snapshot, nil, ErrIntegrity
	}
	if native.blocks != nil {
		snapshot.parser = native.blocks[field]
	}
	active := native.blockActive && native.field == field
	if snapshot.parser == nil {
		if active {
			return snapshot, nil, ErrIntegrity
		}
		if snapshot.paged && snapshot.heap.bytes != 0 {
			var empty docRangeHeaderHeapEstimate
			empty.allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil))))
			if snapshot.heap.bytes != empty.bytes {
				return snapshot, nil, ErrIntegrity
			}
		}
		return snapshot, values, nil
	}
	parser := snapshot.parser
	if parser.paged != native.pagedEvents || parser.parentDepth != native.parentDepth+1 || active && parser.frame == nil ||
		parser.frame != nil && parser.frame != scanner && (parser.frame != native.checkpointScanner || *parser.frame != *scanner) ||
		!active && parser.phase != legacyNativeBlocksDone {
		return snapshot, nil, ErrIntegrity
	}
	if active && (native.phase != legacyNativeObjectApply || native.skipValue || native.valueFirst != '[' && native.valueFirst != 'n' ||
		parser.phase == legacyNativeBlocksDone || !parser.prefix || parser.position < native.valueStart ||
		parser.end != native.valueEnd || native.valueEnd != native.end || native.position != native.valueStart ||
		parser.position < max(0, parser.end-3*parser.maximum-2)) {
		return snapshot, nil, ErrIntegrity
	}
	var err error
	snapshot.parserShape, err = legacyNativeCheckpointBlockListControlFor(parser, binding, sourceBytes)
	snapshot.frame = parser.frame
	return snapshot, values, err
}

func legacyNativeObjectCheckpointBlockParserHeap(parser *legacyNativeBlockList) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if parser != nil {
		parser.retained(&heap)
		heap.include(parser.estimate)
		heap.include(legacyNativeBlocksBacking(cap(parser.previous)))
	}
	return heap
}

type legacyNativeObjectBlockCheckpoint struct {
	native       *legacyNativeObject
	scanner      *legacyJSONValueScanner
	scannerShape legacyJSONValueScanner
	snapshot     legacyNativeObjectBlockCheckpointSnapshot
	values       []ContentBlock
	state        legacyNativeObjectCheckpointBlockField
	binding      ContentHash
	source       LegacyByteSource
	store        legacyNativeCheckpointIO
	rows         legacyNativeCheckpointListWriter[legacyNativeCheckpointBlockRecord]
	record       *legacyNativeCheckpointBlockRecordWriter
	child        *legacyNativeBlockListCheckpoint
	sourceHeap   docRangeHeaderHeapEstimate
	field        int
	index        int
	phase        uint8
	budget       int64
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeObjectBlockCheckpoint(ctx context.Context, native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectBlockCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Put == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	snapshot, values, err := legacyNativeObjectBlockCheckpointSnapshotFor(native, field, scanner, binding, source.Bytes)
	if err != nil {
		return nil, err
	}
	state := legacyNativeObjectCheckpointBlockField{Destination: snapshot.shape,
		Binding: legacyNativeObjectCheckpointBlocksBinding(values, snapshot.parser), Initialized: snapshot.heap.bytes != 0, Paged: native.pagedEvents}
	var sourceHeap docRangeHeaderHeapEstimate
	sourceHeap.include(snapshot.heap)
	sourceHeap.include(legacyNativeBlocksBacking(cap(values)))
	sourceHeap.include(legacyNativeObjectCheckpointBlockParserHeap(snapshot.parser))
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectBlockCheckpoint{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(binding)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	planned.include(sourceHeap)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	return &legacyNativeObjectBlockCheckpoint{native: native, scanner: scanner, scannerShape: *scanner, snapshot: snapshot,
		values: values, state: state, binding: binding, source: source, store: store, sourceHeap: sourceHeap, field: field, budget: budget}, nil
}

func (work *legacyNativeObjectBlockCheckpoint) checkSource() error {
	if work.scanner == nil || *work.scanner != work.scannerShape {
		return ErrConflict
	}
	current, _, err := legacyNativeObjectBlockCheckpointSnapshotFor(work.native, work.field, work.scanner, work.binding, work.source.Bytes)
	if err != nil {
		return err
	}
	if current != work.snapshot {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeObjectBlockCheckpoint) writeRows(ctx context.Context) error {
	if work.index == cap(work.values) {
		tree, err := work.rows.finish(ctx, work.store)
		if err != nil {
			return err
		}
		work.state.Destination.Records = tree
		work.rows.close()
		work.phase++
		return nil
	}
	if work.record == nil {
		value := legacyNativeBlockValue{block: work.values[:cap(work.values)][work.index]}
		work.sourceHeap.allocation(int64(len(value.block.Type)), 1)
		work.sourceHeap.allocation(int64(len(value.block.Text)), 1)
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		work.record, err = newLegacyNativeCheckpointBlockRecordWriter(ctx, value, false, work.store, remaining)
		return err
	}
	ready, err := work.record.step(ctx)
	if err != nil || !ready {
		return err
	}
	row, err := work.record.take(ctx)
	work.record = nil
	if err != nil {
		return err
	}
	if err := work.rows.append(ctx, work.store, row); err != nil {
		return err
	}
	work.index++
	return nil
}

func (work *legacyNativeObjectBlockCheckpoint) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if work.snapshot.parser == nil {
			work.phase++
			return nil
		}
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.child, err = newLegacyNativeBlockListCheckpoint(ctx, work.snapshot.parser, work.binding, work.source, work.store, remaining)
			return err
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Parser, err = work.child.take(ctx)
		work.child = nil
		if err == nil {
			work.phase++
		}
		return err
	case 1:
		if work.state.Binding != 0 {
			work.phase++
			return nil
		}
		return work.writeRows(ctx)
	case 2:
		if err := work.state.validate(work.source.Bytes); err != nil {
			return err
		}
		work.ready = true
		return nil
	}
	return ErrIntegrity
}

func (work *legacyNativeObjectBlockCheckpoint) step(ctx context.Context) (bool, error) {
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
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if !work.ready {
		if err := work.advance(ctx); err != nil {
			return work.fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeObjectBlockCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.binding)+len(work.state.Parser.Hash)+len(work.state.Destination.Records.Ref.Hash)), 1)
	retained, err := work.rows.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.record.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.child.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectBlockCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if work.sourceHeap.err != nil {
		return 0, work.sourceHeap.err
	}
	if retained < 0 || retained > work.budget || work.sourceHeap.bytes > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - work.sourceHeap.bytes, nil
}

func (work *legacyNativeObjectBlockCheckpoint) admit(extra int64) error {
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

func (work *legacyNativeObjectBlockCheckpoint) take(ctx context.Context) (legacyNativeObjectCheckpointBlockField, error) {
	if work == nil {
		return legacyNativeObjectCheckpointBlockField{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectCheckpointBlockField{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointBlockField{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectCheckpointBlockField{}, work.failure
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeObjectCheckpointBlockField{}, work.failure
	}
	state := work.state
	work.close()
	return state, nil
}

func (work *legacyNativeObjectBlockCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectBlockCheckpoint) close() {
	if work != nil {
		work.rows.close()
		work.record.close()
		work.child.close()
		failure := work.failure
		*work = legacyNativeObjectBlockCheckpoint{closed: true, failure: failure}
	}
}

type legacyNativeObjectBlockCheckpointRestore struct {
	state        legacyNativeObjectCheckpointBlockField
	binding      ContentHash
	source       LegacyByteSource
	store        legacyNativeCheckpointIO
	result       legacyNativeObjectBlockRestored
	rows         *legacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord]
	record       *legacyNativeCheckpointBlockRecordRestore
	child        *legacyNativeBlockListCheckpointRestore
	physical     docRangeHeaderHeapEstimate
	aliasPayload docRangeHeaderHeapEstimate
	index        int
	phase        uint8
	budget       int64
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeObjectBlockCheckpointRestore(ctx context.Context, state legacyNativeObjectCheckpointBlockField, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeObjectBlockCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.Read == nil || budget <= 0 || !legacyNativeCheckpointBlockListHashValid(binding) {
		return nil, ErrIntegrity
	}
	if err := state.validate(source.Bytes); err != nil {
		return nil, err
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectBlockCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(int64(len(binding)+len(state.Parser.Hash)+len(state.Destination.Records.Ref.Hash)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	parts := make([]ConversationManifestChunk, len(source.Parts))
	copy(parts, source.Parts)
	source.Parts = parts
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source.Parts[index].Hash = ContentHash(strings.Clone(string(source.Parts[index].Hash)))
	}
	state.Parser.Hash = ContentHash(strings.Clone(string(state.Parser.Hash)))
	state.Destination.Records.Ref.Hash = ContentHash(strings.Clone(string(state.Destination.Records.Ref.Hash)))
	return &legacyNativeObjectBlockCheckpointRestore{state: state, binding: ContentHash(strings.Clone(string(binding))), source: source, store: store, budget: budget}, nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) allocate() error {
	if work.state.Binding == 3 {
		if work.result.Parser == nil || !work.result.Parser.paged {
			return ErrIntegrity
		}
		work.phase = 4
		return nil
	}
	values := work.state.Destination
	physical := legacyNativeBlocksBacking(values.Capacity)
	physical.allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil))))
	if physical.err != nil {
		return physical.err
	}
	if err := work.admit(physical.bytes); err != nil {
		return err
	}
	if work.state.Binding == 0 {
		if !values.Nil {
			work.result.Values = make([]ContentBlock, values.Length, values.Capacity)
		}
		work.physical = physical
		work.phase++
		return nil
	}
	parser := work.result.Parser
	if parser == nil || parser.paged {
		return ErrIntegrity
	}
	backing := parser.decoded
	if work.state.Binding == 2 {
		backing = parser.previous
	}
	if values.Nil != (backing == nil) || values.Capacity != cap(backing) {
		return ErrIntegrity
	}
	work.result.Values = backing[:values.Length:values.Capacity]
	work.physical = physical
	work.phase = 3
	return nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) includeRow(row legacyNativeBlockValue) error {
	for ordinal, heap := range row.heaps {
		work.physical.include(heap)
		work.aliasPayload.include(heap)
		if work.state.Initialized && work.index < work.state.Destination.Length {
			var excess docRangeHeaderHeapEstimate
			excess.include(docRangeHeaderHeapEstimate{bytes: work.result.Excess})
			excess.include(docRangeHeaderHeapEstimate{bytes: row.excess[ordinal]})
			if excess.err != nil {
				return excess.err
			}
			work.result.Excess = excess.bytes
		}
	}
	if work.physical.err != nil {
		return work.physical.err
	}
	return work.aliasPayload.err
}

func (work *legacyNativeObjectBlockCheckpointRestore) readRows(ctx context.Context) error {
	tree := work.state.Destination.Records
	if work.rows == nil {
		if tree.Count == 0 {
			work.phase = 4
			return nil
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord]{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeCursor{})))
		planned.allocation(int64(len(tree.Ref.Hash)), 1)
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		var err error
		work.rows, err = newLegacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord](tree, work.store)
		return err
	}
	if work.record == nil {
		row, present, err := work.rows.next(ctx)
		if err != nil {
			return err
		}
		if !present {
			if work.index != work.state.Destination.Capacity {
				return ErrIntegrity
			}
			work.rows.close()
			work.rows = nil
			work.phase = 4
			return nil
		}
		if work.index >= work.state.Destination.Capacity {
			return ErrIntegrity
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		work.record, err = newLegacyNativeCheckpointBlockRecordRestore(ctx, row, work.store, work.source.Bytes, remaining)
		return err
	}
	ready, err := work.record.step(ctx)
	if err != nil || !ready {
		return err
	}
	row, err := work.record.take(ctx)
	work.record = nil
	if err != nil {
		return err
	}
	work.result.Values[:cap(work.result.Values)][work.index] = row.block
	if err := work.includeRow(row); err != nil {
		return err
	}
	work.index++
	return nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) readAlias(ctx context.Context) error {
	parser := work.result.Parser
	if parser == nil {
		return ErrIntegrity
	}
	heaps, excess := parser.payloads, parser.excess
	if work.state.Binding == 2 {
		heaps, excess = parser.previousHeaps, parser.previousExtra
	}
	capacity := work.state.Destination.Capacity
	if len(heaps) != 2*capacity || len(excess) != 2*capacity {
		return ErrIntegrity
	}
	limit := min(capacity, work.index+legacyNativeCheckpointPageItems)
	for work.index < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := legacyNativeBlockValue{block: work.result.Values[:capacity][work.index]}
		copy(row.heaps[:], heaps[2*work.index:2*(work.index+1)])
		copy(row.excess[:], excess[2*work.index:2*(work.index+1)])
		if err := work.includeRow(row); err != nil {
			return err
		}
		work.index++
	}
	if work.index == capacity {
		work.phase++
	}
	return nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) finish() error {
	parser := work.result.Parser
	if work.state.Initialized {
		work.result.Heap = work.physical
		if work.state.Binding == 3 {
			work.result.Heap = parser.heap()
			work.result.Excess = parser.visibleExcess
		}
		if work.result.Heap.err != nil || work.result.Heap.bytes <= 0 || work.result.Excess < 0 {
			return ErrIntegrity
		}
	}
	if parser != nil {
		parser.borrowed = work.result.Heap
		if work.state.Initialized && (work.state.Binding == 1 || work.state.Binding == 2) {
			if parser.added.err != nil || parser.added.bytes < work.aliasPayload.bytes {
				return ErrIntegrity
			}
			parser.added.bytes -= work.aliasPayload.bytes
			if work.state.Binding == 1 {
				parser.owned = false
			} else {
				parser.previousOwned = false
			}
		}
	}
	work.ready = true
	return nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if work.state.Parser == (legacyNativeCheckpointRef{}) {
			work.phase++
			return nil
		}
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.child, err = newLegacyNativeBlockListCheckpointRestore(ctx, work.state.Parser, work.store, work.source, work.binding, remaining)
			return err
		}
		ready, err := work.child.step(ctx)
		if err != nil || !ready {
			return err
		}
		value, err := work.child.take(ctx)
		work.child = nil
		if err != nil {
			return err
		}
		if value.List == nil || value.List.paged != work.state.Paged || value.Source.authority != work.source.authority || value.Source.Bytes != work.source.Bytes {
			return ErrIntegrity
		}
		work.result.Parser, work.source = value.List, value.Source
		work.phase++
		return nil
	case 1:
		return work.allocate()
	case 2:
		return work.readRows(ctx)
	case 3:
		return work.readAlias(ctx)
	case 4:
		return work.finish()
	}
	return ErrIntegrity
}

func (work *legacyNativeObjectBlockCheckpointRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
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

func (work *legacyNativeObjectBlockCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.binding)+len(work.state.Parser.Hash)+len(work.state.Destination.Records.Ref.Hash)), 1)
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.include(work.physical)
	heap.include(legacyNativeObjectCheckpointBlockParserHeap(work.result.Parser))
	if work.rows != nil {
		retained, err := work.rows.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	retained, err := work.record.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.child.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectBlockCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) admit(extra int64) error {
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

func (work *legacyNativeObjectBlockCheckpointRestore) take(ctx context.Context) (legacyNativeObjectBlockRestored, error) {
	if work == nil {
		return legacyNativeObjectBlockRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeObjectBlockRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeObjectBlockRestored{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeObjectBlockRestored{}, work.failure
	}
	result := work.result
	result.Source = work.source
	work.close()
	return result, nil
}

func (work *legacyNativeObjectBlockCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeObjectBlockCheckpointRestore) close() {
	if work != nil {
		work.rows.close()
		work.record.close()
		work.child.close()
		failure := work.failure
		*work = legacyNativeObjectBlockCheckpointRestore{closed: true, failure: failure}
	}
}
