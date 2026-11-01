package domain

import (
	"context"
	"reflect"
	"strings"
	"unsafe"
)

func legacyNativeBlockListCheckpointSource(ctx context.Context, source LegacyByteSource) error {
	if err := legacyNativeAnyCheckpointSourceGeometry(source, source.Bytes); err != nil {
		return err
	}
	for index := range source.Parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(source.Parts[index].Hash) != len(contentHashPrefix)+64 {
			return ErrIntegrity
		}
		if err := legacyNativeAnyCheckpointSourcePart(source, index); err != nil {
			return err
		}
	}
	return nil
}

func legacyNativeBlockListCheckpointStateHeap(state legacyNativeCheckpointBlockListState) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	for _, hash := range []ContentHash{state.Binding, state.Schema, state.Decoded.Records.Ref.Hash, state.Previous.Records.Ref.Hash, state.DataRecords.Ref.Hash, state.Frame.Hash, state.Object.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	return heap
}

type legacyNativeBlockListCheckpoint struct {
	list       *legacyNativeBlockList
	shape      legacyNativeCheckpointBlockListState
	state      legacyNativeCheckpointBlockListState
	frame      *legacyJSONValueScanner
	frameShape legacyJSONValueScanner
	emptyFrame legacyJSONValueScanner
	object     *legacyNativeObject
	source     LegacyByteSource
	store      legacyNativeCheckpointIO
	rows       legacyNativeCheckpointListWriter[legacyNativeCheckpointBlockRecord]
	record     legacyNativeAnyCheckpointTask[legacyNativeCheckpointBlockRecord]
	child      legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef]
	root       legacyNativeCheckpointRef
	index      int
	phase      uint8
	budget     int64
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeBlockListCheckpoint(ctx context.Context, list *legacyNativeBlockList, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, budget int64) (*legacyNativeBlockListCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if list == nil || store.Put == nil || budget <= 0 || len(binding) != len(contentHashPrefix)+64 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	state, err := legacyNativeCheckpointBlockListControlFor(list, binding, source.Bytes)
	if err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockListCheckpoint{})))
	planned.include(legacyNativeBlockListCheckpointStateHeap(state))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	list.retained(&planned)
	planned.include(list.estimate)
	planned.include(legacyNativeBlocksBacking(cap(list.previous)))
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeBlockListCheckpoint{list: list, shape: state, state: state, frame: list.frame, object: list.object, source: source, store: store, budget: budget}
	if list.frame != nil {
		work.frameShape = *list.frame
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeBlockListCheckpoint) checkSource() error {
	if work.list == nil || work.list.object != work.object || work.list.frame != work.frame {
		return ErrConflict
	}
	if work.frame != nil && *work.frame != work.frameShape {
		return ErrConflict
	}
	current, err := legacyNativeCheckpointBlockListControlFor(work.list, work.state.Binding, work.source.Bytes)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, work.shape) {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeBlockListCheckpoint) recordAt(index int) (legacyNativeBlockValue, bool, error) {
	if work.phase == 2 {
		if work.list.data == nil {
			return legacyNativeBlockValue{}, false, ErrIntegrity
		}
		value := work.list.data.records.at(index)
		if value == nil {
			return legacyNativeBlockValue{}, false, ErrIntegrity
		}
		return *value, true, nil
	}
	values, heaps, excess := work.list.decoded, work.list.payloads, work.list.excess
	initialized := work.list.initialized
	if work.phase == 1 {
		values, heaps, excess = work.list.previous, work.list.previousHeaps, work.list.previousExtra
		initialized = true
	}
	if index < 0 || index >= cap(values) {
		return legacyNativeBlockValue{}, false, ErrIntegrity
	}
	value := legacyNativeBlockValue{block: values[:cap(values)][index]}
	if initialized {
		if len(heaps) < 2*(index+1) || len(excess) < 2*(index+1) {
			return value, false, ErrIntegrity
		}
		copy(value.heaps[:], heaps[2*index:2*(index+1)])
		copy(value.excess[:], excess[2*index:2*(index+1)])
	}
	return value, initialized, nil
}

func (work *legacyNativeBlockListCheckpoint) writeRows(ctx context.Context) error {
	length := work.state.Decoded.Capacity
	if work.phase == 1 {
		length = work.state.Previous.Capacity
	}
	if work.phase == 2 {
		length = int(work.state.DataRecords.Count)
	}
	if work.index == length {
		tree, err := work.rows.finish(ctx, work.store)
		if err != nil {
			return err
		}
		switch work.phase {
		case 0:
			work.state.Decoded.Records = tree
		case 1:
			work.state.Previous.Records = tree
		case 2:
			work.state.DataRecords = tree
		}
		work.rows.close()
		work.rows = legacyNativeCheckpointListWriter[legacyNativeCheckpointBlockRecord]{}
		work.index = 0
		work.phase++
		return nil
	}
	if work.record == nil {
		value, initialized, err := work.recordAt(work.index)
		if err != nil {
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		work.record, err = newLegacyNativeCheckpointBlockRecordWriter(ctx, value, initialized, work.store, remaining)
		return err
	}
	ready, err := work.record.step(ctx)
	if err != nil || !ready {
		return err
	}
	value, err := work.record.take(ctx)
	work.record = nil
	if err != nil {
		return err
	}
	if err := work.rows.append(ctx, work.store, value); err != nil {
		return err
	}
	work.index++
	return nil
}

func (work *legacyNativeBlockListCheckpoint) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	var err error
	switch work.phase {
	case 0, 1, 2:
		err = work.writeRows(ctx)
	case 3:
		if work.frame != nil {
			var body []byte
			body, err = work.frame.checkpoint(ctx)
			if err == nil {
				work.state.Frame, err = legacyNativeCheckpointWrite(ctx, work.store, body)
			}
		}
		if err == nil {
			work.phase++
		}
	case 4:
		if work.object == nil {
			work.phase++
			break
		}
		if work.child == nil {
			var remaining int64
			remaining, err = work.remaining()
			frame := work.frame
			if frame == nil {
				frame = &work.emptyFrame
			}
			if err == nil {
				work.child, err = newLegacyNativeObjectCheckpoint(ctx, work.object, frame, work.state.Binding, work.source, work.store, remaining)
			}
			break
		}
		var ready bool
		ready, err = work.child.step(ctx)
		if err == nil && ready {
			work.state.Object, err = work.child.take(ctx)
			work.child = nil
			if err == nil {
				work.phase++
			}
		}
	case 5:
		if err = legacyNativeCheckpointBlockListValidate(work.state, work.state.Binding, work.source.Bytes, true); err == nil {
			work.root, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
			work.ready = err == nil
		}
	default:
		err = ErrIntegrity
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = work.admit(0)
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeBlockListCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeBlockListCheckpointStateHeap(work.state))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.root.Hash)), 1)
	retained, err := work.rows.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.record, work.child} {
		if current != nil {
			retained, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	return heap.bytes, heap.err
}
func (work *legacyNativeBlockListCheckpoint) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	var source docRangeHeaderHeapEstimate
	if work.list == nil {
		return 0, ErrIntegrity
	}
	work.list.retained(&source)
	source.include(work.list.estimate)
	source.include(legacyNativeBlocksBacking(cap(work.list.previous)))
	if err != nil {
		return 0, err
	}
	if source.err != nil {
		return 0, source.err
	}
	if retained < 0 || retained > work.budget || source.bytes > work.budget-retained {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained - source.bytes, nil
}
func (work *legacyNativeBlockListCheckpoint) admit(extra int64) error {
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
func (work *legacyNativeBlockListCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}
func (work *legacyNativeBlockListCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
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
	value := work.root
	work.close()
	return value, nil
}
func (work *legacyNativeBlockListCheckpoint) close() {
	if work != nil {
		work.rows.close()
		for _, current := range []interface{ close() }{work.record, work.child} {
			if current != nil {
				current.close()
			}
		}
		failure := work.failure
		*work = legacyNativeBlockListCheckpoint{closed: true, failure: failure}
	}
}

type legacyNativeBlockListCheckpointRestore struct {
	state    legacyNativeCheckpointBlockListState
	ref      legacyNativeCheckpointRef
	binding  ContentHash
	source   LegacyByteSource
	store    legacyNativeCheckpointIO
	list     *legacyNativeBlockList
	rows     *legacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord]
	record   legacyNativeAnyCheckpointTask[legacyNativeBlockValue]
	child    legacyNativeAnyCheckpointTask[legacyNativeObjectRestored]
	physical [3]docRangeHeaderHeapEstimate
	logical  docRangeHeaderHeapEstimate
	index    int
	stream   uint8
	phase    uint8
	checked  int
	field    int
	compared int
	budget   int64
	ready    bool
	closed   bool
	failure  error
}

func newLegacyNativeBlockListCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, source LegacyByteSource, binding ContentHash, budget int64) (*legacyNativeBlockListCheckpointRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > 16384 || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeBlockListCheckpointSource(ctx, source); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockListCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSourceHeap(source))
	planned.allocation(legacyNativeAnyCheckpointScratch+int64(16*ref.Bytes), 1)
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
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	return &legacyNativeBlockListCheckpointRestore{ref: ref, binding: ContentHash(strings.Clone(string(binding))), source: source, store: store, budget: budget}, nil
}

func legacyNativeBlockListCheckpointArrayHeap(capacity int) docRangeHeaderHeapEstimate {
	heap := legacyNativeBlocksBacking(capacity)
	heap.allocation(2*int64(capacity), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	heap.allocation(2*int64(capacity), int64(unsafe.Sizeof(int64(0))))
	return heap
}

func (work *legacyNativeBlockListCheckpointRestore) allocate() error {
	state := work.state
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockList{})))
	planned.include(legacyNativeBlockListCheckpointArrayHeap(state.Decoded.Capacity))
	planned.include(legacyNativeBlockListCheckpointArrayHeap(state.Previous.Capacity))
	if state.HasData {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockValues{})))
	}
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return err
	}
	list := &legacyNativeBlockList{position: state.Position, end: state.End, parentDepth: state.ParentDepth, maximum: state.Maximum, count: state.Count, copied: state.Copied, childStart: state.ChildStart, literal: state.Literal, seeded: state.Decoded.Capacity, phase: state.Phase, prefix: state.Prefix, paged: state.Paged, initialized: true}
	if !state.Decoded.Nil {
		list.decoded = make([]ContentBlock, state.Decoded.Length, state.Decoded.Capacity)
		list.payloads = make([]docRangeHeaderHeapEstimate, 2*state.Decoded.Capacity)
		list.excess = make([]int64, 2*state.Decoded.Capacity)
	}
	if !state.Previous.Nil {
		list.previous = make([]ContentBlock, state.Previous.Length, state.Previous.Capacity)
		list.previousHeaps = make([]docRangeHeaderHeapEstimate, 2*state.Previous.Capacity)
		list.previousExtra = make([]int64, 2*state.Previous.Capacity)
	}
	if state.HasData {
		list.data = &legacyNativeBlockValues{}
	}
	list.owned, list.previousOwned = cap(list.decoded) > 0, cap(list.previous) > 0
	work.list = list
	return nil
}

func (work *legacyNativeBlockListCheckpointRestore) readRows(ctx context.Context) error {
	tree := work.state.Decoded.Records
	if work.stream == 1 {
		tree = work.state.Previous.Records
	}
	if work.stream == 2 {
		tree = work.state.DataRecords
	}
	if work.rows == nil {
		if tree.Count == 0 {
			work.stream++
			work.index = 0
			return nil
		}
		var err error
		work.rows, err = newLegacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord](tree, work.store)
		return err
	}
	if work.record == nil {
		row, ok, err := work.rows.next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			if uint64(work.index) != tree.Count {
				return ErrIntegrity
			}
			work.rows.close()
			work.rows = nil
			work.index = 0
			work.stream++
			return nil
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
	if work.stream == 2 {
		bytes, err := work.list.data.records.appendBytes()
		if err != nil {
			return err
		}
		if err := work.admit(int64(bytes)); err != nil {
			return err
		}
	}
	row, err := work.record.take(ctx)
	work.record = nil
	if err != nil {
		return err
	}
	for _, heap := range row.heaps {
		work.physical[work.stream].include(heap)
	}
	if work.physical[work.stream].err != nil {
		return work.physical[work.stream].err
	}
	switch work.stream {
	case 0:
		work.list.decoded[:cap(work.list.decoded)][work.index] = row.block
		copy(work.list.payloads[2*work.index:2*(work.index+1)], row.heaps[:])
		copy(work.list.excess[2*work.index:2*(work.index+1)], row.excess[:])
	case 1:
		work.list.previous[:cap(work.list.previous)][work.index] = row.block
		copy(work.list.previousHeaps[2*work.index:2*(work.index+1)], row.heaps[:])
		copy(work.list.previousExtra[2*work.index:2*(work.index+1)], row.excess[:])
	case 2:
		if err := work.list.data.records.append(row); err != nil {
			return err
		}
		work.list.data.count = min(work.index+1, work.state.DataCount)
	default:
		return ErrIntegrity
	}
	work.index++
	return nil
}

func legacyNativeBlockListCheckpointCompare(ctx context.Context, left, right ContentBlock, field, offset *int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if *field >= 2 {
		return true, nil
	}
	metadata := legacyContentBlockNativeFields.fields[*field]
	first, second := reflect.ValueOf(left).Field(metadata.index).String(), reflect.ValueOf(right).Field(metadata.index).String()
	if len(first) != len(second) || *offset < 0 || *offset > len(first) {
		return false, ErrIntegrity
	}
	length := min(len(first)-*offset, LegacyCanonicalEncodingBytes)
	if first[*offset:*offset+length] != second[*offset:*offset+length] {
		return false, ErrIntegrity
	}
	*offset += length
	if *offset == len(first) {
		*field++
		*offset = 0
	}
	return *field == 2, nil
}

func (work *legacyNativeBlockListCheckpointRestore) normalizeMigration(ctx context.Context) (bool, error) {
	list := work.list
	if list.previous == nil || work.checked == list.copied {
		return true, nil
	}
	index := work.checked
	left, right := list.decoded[:cap(list.decoded)][index], list.previous[index]
	if list.payloads[2*index] != list.previousHeaps[2*index] || list.payloads[2*index+1] != list.previousHeaps[2*index+1] || list.excess[2*index] != list.previousExtra[2*index] || list.excess[2*index+1] != list.previousExtra[2*index+1] {
		return false, ErrIntegrity
	}
	ready, err := legacyNativeBlockListCheckpointCompare(ctx, left, right, &work.field, &work.compared)
	if err != nil || !ready {
		return false, err
	}
	for _, heap := range list.payloads[2*index : 2*(index+1)] {
		if work.physical[0].bytes < heap.bytes {
			return false, ErrIntegrity
		}
		work.physical[0].bytes -= heap.bytes
	}
	list.decoded[index] = right
	work.checked++
	work.field = 0
	return work.checked == list.copied, nil
}

func (work *legacyNativeBlockListCheckpointRestore) normalizeChild(ctx context.Context) (bool, error) {
	list := work.list
	if list.object == nil {
		return work.state.BlockAliases == 0, nil
	}
	prior, err := list.blockRecord()
	if err != nil {
		return false, err
	}
	if work.field == 2 {
		return true, nil
	}
	if work.state.BlockAliases&(1<<work.field) == 0 {
		work.field++
		return work.field == 2, nil
	}
	metadata := legacyContentBlockNativeFields.fields[work.field]
	left, right := reflect.ValueOf(list.block).Field(metadata.index).String(), reflect.ValueOf(prior.block).Field(metadata.index).String()
	if len(left) != len(right) || work.compared > len(left) {
		return false, ErrIntegrity
	}
	length := min(len(left)-work.compared, LegacyCanonicalEncodingBytes)
	if left[work.compared:work.compared+length] != right[work.compared:work.compared+length] {
		return false, ErrIntegrity
	}
	work.compared += length
	if work.compared == len(left) {
		reflect.ValueOf(&list.block).Elem().Field(metadata.index).SetString(right)
		heap := prior.heaps[work.field]
		heap.allocation(1, int64(unsafe.Sizeof(string(""))))
		if heap.err != nil {
			return false, heap.err
		}
		list.object.heaps[work.field], list.object.backingExcess[work.field] = heap, prior.excess[work.field]
		work.field++
		work.compared = 0
	}
	return work.field == 2, ctx.Err()
}

func (work *legacyNativeBlockListCheckpointRestore) logicalRow(index int) (legacyNativeBlockValue, error) {
	list := work.list
	if list.paged {
		value := list.data.records.at(index)
		if value == nil {
			return legacyNativeBlockValue{}, ErrIntegrity
		}
		return *value, nil
	}
	values, heaps, excess := list.decoded[:cap(list.decoded)], list.payloads, list.excess
	if list.previous != nil && index >= list.copied && index < len(list.previous) {
		values, heaps, excess = list.previous, list.previousHeaps, list.previousExtra
	}
	if index >= len(values) {
		return legacyNativeBlockValue{}, ErrIntegrity
	}
	value := legacyNativeBlockValue{block: values[index]}
	copy(value.heaps[:], heaps[2*index:2*(index+1)])
	copy(value.excess[:], excess[2*index:2*(index+1)])
	return value, nil
}

func (work *legacyNativeBlockListCheckpointRestore) normalizePayload(ctx context.Context) (bool, error) {
	list := work.list
	length := cap(list.decoded)
	if list.paged && list.data != nil {
		length = list.data.records.length()
	}
	limit := min(length, work.index+legacyNativeCheckpointPageItems)
	for work.index < limit {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		row, err := work.logicalRow(work.index)
		if err != nil {
			return false, err
		}
		for field, heap := range row.heaps {
			work.logical.include(heap)
			var excess docRangeHeaderHeapEstimate
			excess.include(docRangeHeaderHeapEstimate{bytes: list.totalExcess})
			excess.include(docRangeHeaderHeapEstimate{bytes: row.excess[field]})
			if excess.err != nil {
				return false, excess.err
			}
			list.totalExcess = excess.bytes
			if work.index < list.count {
				visible := docRangeHeaderHeapEstimate{bytes: list.visibleExcess}
				visible.include(docRangeHeaderHeapEstimate{bytes: row.excess[field]})
				if visible.err != nil {
					return false, visible.err
				}
				list.visibleExcess = visible.bytes
			}
		}
		work.index++
	}
	if work.logical.err != nil {
		return false, work.logical.err
	}
	if work.index != length {
		return false, nil
	}
	list.payload = work.logical
	if list.paged {
		list.refreshPagedHeap()
	} else {
		list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
		list.estimate.include(legacyNativeBlocksBacking(cap(list.decoded)))
		list.estimate.include(list.payload)
		list.added = work.physical[0]
		list.added.include(work.physical[1])
	}
	return true, list.estimate.err
}

func (work *legacyNativeBlockListCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if err := work.admit(int64(16 * work.ref.Bytes)); err != nil {
			return err
		}
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return err
		}
		if err := legacyNativeCheckpointBlockListValidate(work.state, work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		work.phase++
	case 1:
		if err := work.allocate(); err != nil {
			return err
		}
		work.phase++
	case 2:
		if work.stream == 3 {
			work.phase++
			work.index = 0
			return nil
		}
		return work.readRows(ctx)
	case 3:
		if work.state.HasFrame {
			body, err := legacyNativeCheckpointRead(ctx, work.store, work.state.Frame)
			if err != nil {
				return err
			}
			frame, err := restoreLegacyJSONValueScanner(ctx, body)
			if err != nil {
				return err
			}
			var frameHeap docRangeHeaderHeapEstimate
			frameHeap.allocation(1, int64(unsafe.Sizeof(frame)))
			if err := work.admit(frameHeap.bytes); err != nil {
				return err
			}
			work.list.frame = &frame
			work.list.ownedFrame = true
		}
		work.phase++
	case 4:
		if work.state.Object == (legacyNativeCheckpointRef{}) {
			work.phase++
			return nil
		}
		if work.child == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			work.child, err = newLegacyNativeObjectCheckpointRestore(ctx, work.state.Object, legacyNativeCheckpointBlockModel, work.store, work.source, work.binding, remaining)
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
		if value.Source.authority != work.source.authority || value.Object == nil {
			return ErrIntegrity
		}
		if work.list.frame != nil {
			if *work.list.frame != value.Scanner {
				return ErrIntegrity
			}
		} else if value.Scanner != (legacyJSONValueScanner{}) {
			return ErrIntegrity
		}
		work.list.block = *value.Object.destination.Interface().(*ContentBlock)
		work.list.object = value.Object
		work.list.object.destination = reflect.ValueOf(&work.list.block)
		work.source = value.Source
		work.phase++
	case 5:
		ready, err := work.normalizeMigration(ctx)
		if err != nil {
			return err
		}
		if ready {
			work.phase++
			work.field = 0
			work.compared = 0
		}
	case 6:
		ready, err := work.normalizeChild(ctx)
		if err != nil {
			return err
		}
		if ready {
			work.phase++
			work.index = 0
			work.field = 0
			work.compared = 0
		}
	case 7:
		ready, err := work.normalizePayload(ctx)
		if err != nil {
			return err
		}
		if ready {
			work.phase++
		}
	case 8:
		if _, err := legacyNativeCheckpointBlockListControlFor(work.list, work.binding, work.source.Bytes); err != nil {
			return err
		}
		if work.list.object != nil {
			object := work.list.object
			if object.parentDepth != work.list.parentDepth+1 || object.end > work.list.end || object.valueStart < 0 || object.position < work.list.childStart {
				return ErrIntegrity
			}
		}
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeBlockListCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeBlockListCheckpointStateHeap(work.state))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)), 1)
	if list := work.list; list != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*list)))
		heap.include(legacyNativeBlockListCheckpointArrayHeap(cap(list.decoded)))
		heap.include(legacyNativeBlockListCheckpointArrayHeap(cap(list.previous)))
		if list.data != nil {
			heap.include(list.data.backing())
		}
		for _, value := range work.physical {
			heap.include(value)
		}
		if list.frame != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*list.frame)))
		}
		if list.object != nil {
			retained, err := list.object.retained()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	if work.rows != nil {
		retained, err := work.rows.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.record, work.child} {
		if current != nil {
			retained, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	return heap.bytes, heap.err
}
func (work *legacyNativeBlockListCheckpointRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}
func (work *legacyNativeBlockListCheckpointRestore) admit(extra int64) error {
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
func (work *legacyNativeBlockListCheckpointRestore) step(ctx context.Context) (bool, error) {
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
func (work *legacyNativeBlockListCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}
func (work *legacyNativeBlockListCheckpointRestore) take(ctx context.Context) (legacyNativeBlockListRestored, error) {
	if work == nil {
		return legacyNativeBlockListRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeBlockListRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeBlockListRestored{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeBlockListRestored{}, ErrIntegrity
	}
	value := legacyNativeBlockListRestored{List: work.list, Source: work.source}
	work.close()
	return value, nil
}
func (work *legacyNativeBlockListCheckpointRestore) close() {
	if work != nil {
		work.rows.close()
		for _, current := range []interface{ close() }{work.record, work.child} {
			if current != nil {
				current.close()
			}
		}
		failure := work.failure
		*work = legacyNativeBlockListCheckpointRestore{closed: true, failure: failure}
	}
}
