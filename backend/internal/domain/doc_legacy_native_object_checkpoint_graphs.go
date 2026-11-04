package domain

import (
	"context"
	"reflect"
	"strings"
	"unsafe"
)

func legacyNativeObjectAnyGraphNamespace(native *legacyNativeObject) uint8 {
	if native == nil || native.metadata != &legacyEventNativeFields || len(native.containers) != len(native.metadata.fields) {
		return 0
	}
	count := 0
	for _, parser := range native.containers {
		if parser != nil {
			count++
			if count == 2 {
				return 1
			}
		}
	}
	return 0
}

func legacyNativeObjectGraphBudget(free, private, shared int64) (int64, error) {
	heap := docRangeHeaderHeapEstimate{bytes: free}
	heap.include(docRangeHeaderHeapEstimate{bytes: private})
	heap.include(docRangeHeaderHeapEstimate{bytes: shared})
	return heap.bytes, heap.err
}

func (work *legacyNativeObjectAnyCheckpoint) sharedParentField() *legacyNativeObjectAnyCheckpoint {
	if work.sharedParent != nil {
		return work
	}
	return nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) sharedParentField() *legacyNativeObjectAnyCheckpointRestore {
	if work.sharedParent != nil {
		return work
	}
	return nil
}

func (work *legacyNativeAnyCheckpoint) sharedGraphAny() *legacyNativeAnyCheckpoint {
	if work.sharedField != nil {
		return work
	}
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) sharedGraphAny() *legacyNativeAnyCheckpointRestore {
	if work.sharedField != nil {
		return work
	}
	return nil
}

func (work *legacyNativeObjectAnyCheckpoint) sharedCaptureRoot() (*legacyNativeObjectCheckpoint, error) {
	if work == nil {
		return nil, ErrConflict
	}
	parent := work.sharedParent
	if work.closed || parent == nil || parent.closed || parent.ready || parent.failure != nil ||
		parent.anyTask != work || parent.field != work.field || parent.native != work.native ||
		parent.state.AnyGraphNamespace != 1 || parent.anyGraph == nil || parent.anyGraph != work.identity || parent.anyGraph.closed ||
		work.binding != parent.state.Binding {
		return nil, ErrConflict
	}
	if err := parent.checkSource(); err != nil {
		return nil, err
	}
	if err := legacyNativeObjectAnyCheckpointSourceShape(work.sources.original, parent.source); err != nil {
		return nil, err
	}
	return parent, nil
}

func (work *legacyNativeObjectAnyCheckpointRestore) sharedRestoreRoot() (*legacyNativeObjectCheckpointRestore, error) {
	if work == nil || work.closed {
		return nil, ErrConflict
	}
	parent := work.sharedParent
	if parent == nil || parent.closed || parent.ready || parent.failure != nil || parent.phase != 2 ||
		parent.anyTask != work || parent.field != work.field || parent.native == nil ||
		parent.state.AnyGraphNamespace != 1 || parent.anyGraph == nil || parent.anyGraph != work.registry || parent.anyGraph.closed ||
		work.binding != parent.binding || work.field < 0 || work.field >= len(parent.state.Fields) {
		return nil, ErrConflict
	}
	row := parent.state.Fields[work.field]
	if row.Any == nil || row.Any.Parser != work.state.Parser || row.Any.Binding != work.state.Binding || row.Any.Active != work.state.Active || row.Initialized != work.initialized ||
		parent.native.metadata.fields[work.field].genericMap != work.genericMap || !reflect.DeepEqual(row.Any.Destination, work.state.Destination) {
		return nil, ErrConflict
	}
	if err := legacyNativeObjectAnyCheckpointSourceShape(work.sources.original, parent.source); err != nil {
		return nil, err
	}
	return parent, nil
}

func (work *legacyNativeObjectAnyCheckpoint) refreshSharedBudget() error {
	if work.sharedParent == nil {
		return nil
	}
	parent, err := work.sharedCaptureRoot()
	if err != nil {
		return err
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, 0)
	return err
}

func (work *legacyNativeObjectAnyCheckpointRestore) refreshSharedBudget() error {
	if work.sharedParent == nil {
		return nil
	}
	parent, err := work.sharedRestoreRoot()
	if err != nil {
		return err
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, 0)
	return err
}

func (work *legacyNativeAnyCheckpoint) refreshSharedBudget() error {
	if work.sharedField == nil {
		return nil
	}
	field := work.sharedField
	parent, err := field.sharedCaptureRoot()
	if err != nil || field.child != work || parent != nil && parent.anyGraph != work.sharedGraph {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, 0)
	return err
}

func (work *legacyNativeAnyCheckpointRestore) refreshSharedBudget() error {
	if work.sharedField == nil {
		return nil
	}
	field := work.sharedField
	parent, err := field.sharedRestoreRoot()
	if err != nil || field.child != work || parent != nil && parent.anyGraph != work.sharedRegistry {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	if work.closed || work.binding != parent.binding || work.root != field.state.Parser {
		return ErrConflict
	}
	if err := legacyNativeObjectAnyCheckpointSourceShape(work.source, field.sources.owned); err != nil {
		return err
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, 0)
	return err
}

func (work *legacyNativeCheckpointGraphWriter) refreshSharedBudget() error {
	if work.sharedField == nil {
		return nil
	}
	field := work.sharedField
	parent, err := field.sharedCaptureRoot()
	if err != nil {
		return err
	}
	if parent.anyGraph != work.identity || work.sharedAny != nil && (field.child != work.sharedAny || work.sharedAny.graph != work || work.sharedAny.sharedField != field) || work.sharedAny == nil && field.graph != work {
		return ErrConflict
	}
	if work.sharedAny != nil {
		if err := work.sharedAny.checkSource(); err != nil {
			return err
		}
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	shared, err := work.identity.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, shared)
	return err
}

func (work *legacyNativeCheckpointGraphRestore) refreshSharedBudget() error {
	if work.sharedField == nil {
		return nil
	}
	field := work.sharedField
	parent, err := field.sharedRestoreRoot()
	if err != nil {
		return err
	}
	if parent.anyGraph != work.registry || work.sharedAny != nil && (field.child != work.sharedAny || work.sharedAny.graph != work || work.sharedAny.sharedField != field) || work.sharedAny == nil && field.graph != work {
		return ErrConflict
	}
	if work.sharedAny != nil {
		if err := work.sharedAny.refreshSharedBudget(); err != nil {
			return err
		}
	}
	free, err := parent.remaining()
	if err != nil {
		return err
	}
	private, err := work.retainedBytes()
	if err != nil {
		return err
	}
	shared, err := work.registry.retainedBytes()
	if err != nil {
		return err
	}
	work.budget, err = legacyNativeObjectGraphBudget(free, private, shared)
	return err
}

func (work *legacyNativeCheckpointGraphRestore) refreshSharedTextBudget(ctx context.Context) error {
	if work.sharedField == nil {
		return nil
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch, 0); err != nil {
		return err
	}
	free, err := work.available()
	if err != nil {
		return err
	}
	text := work.text
	retained, err := text.retainedBytes()
	if err != nil {
		return err
	}
	text.budget, err = legacyNativeObjectGraphBudget(free, retained, 0)
	if err != nil || text.inner == nil {
		return err
	}
	inner := text.inner
	retained, err = inner.retainedBytes()
	if err != nil {
		return err
	}
	inner.budget, err = legacyNativeObjectGraphBudget(free, retained, 0)
	if err != nil {
		return err
	}
	next := docRangeHeaderHeapEstimate{bytes: retained}
	if !inner.ready && inner.scalar == nil {
		next.allocation(1, int64(unsafe.Sizeof(legacyNativeScalar{})))
		if inner.state.Present {
			next.allocation(1, int64(unsafe.Sizeof(strings.Builder{})))
			next.allocation(legacyNativeScalarAllocationBound(inner.state.Capacity), 1)
		}
	} else if !inner.ready && inner.page == nil && inner.copied < inner.state.Bytes {
		next.allocation(legacyNativeScalarAllocationBound(min(inner.source.partBytes(), inner.source.Bytes-inner.copied)), 1)
	}
	if next.err != nil {
		return next.err
	}
	if next.bytes > inner.budget {
		return ErrDocumentResourceLimit
	}
	return ctx.Err()
}

func (work *legacyNativeAnyCheckpointRestore) sharedParserBytes() (int64, error) {
	if work.sharedField == nil || work.parser == nil {
		return 0, nil
	}
	parser := work.parser
	if parser.live.err != nil || work.frameHeap.err != nil || parser.live.bytes < work.frameHeap.bytes {
		return 0, ErrIntegrity
	}
	shared := docRangeHeaderHeapEstimate{bytes: parser.live.bytes - work.frameHeap.bytes}
	shared.include(docRangeHeaderHeapEstimate{bytes: parser.visible.bytes})
	if parser.rootReserved {
		if parser.rootMap != nil {
			shared.include(parser.rootMap.heap())
			parser.rootMap.retained(&shared)
		}
		if parser.rootArray != nil {
			shared.include(parser.rootArray.heap())
			parser.rootArray.retained(&shared)
		}
	}
	if parser.pending && parser.node.owner != (legacyNativeAnyValueOwner{}) {
		shared.include(parser.node.heap)
		shared.include(docRangeHeaderHeapEstimate{bytes: parser.node.owner.bytes})
	}
	var total docRangeHeaderHeapEstimate
	parser.retained(&total)
	if shared.err != nil || total.err != nil || shared.bytes > total.bytes {
		return 0, ErrIntegrity
	}
	return shared.bytes, nil
}

func (work *legacyNativeObjectCheckpointRestore) checkSharedHandoff(ctx context.Context) error {
	if work.state.AnyGraphNamespace == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	retained, err := work.native.retained()
	heap := docRangeHeaderHeapEstimate{bytes: retained, err: err}
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	if work.native.checkpointScanner == nil {
		heap.allocation(1, int64(unsafe.Sizeof(legacyJSONValueScanner{})))
	}
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}
