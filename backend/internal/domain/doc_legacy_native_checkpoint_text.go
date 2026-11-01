package domain

import (
	"context"
	"unsafe"
)

const (
	legacyNativeCheckpointTextRead uint8 = iota
	legacyNativeCheckpointTextWrite
	legacyNativeCheckpointTextIndex
)

type legacyNativeCheckpointTextWriter struct {
	store    legacyNativeCheckpointIO
	read     func(context.Context, int, int) ([]byte, error)
	tree     legacyNativeCheckpointTreeBuilder
	result   legacyNativeCheckpointText
	length   int
	position int
	part     []byte
	pending  legacyNativeCheckpointRef
	phase    uint8
	ready    bool
	closed   bool
	failure  error
}

func newLegacyNativeCheckpointTextWriter(ctx context.Context, length int, read func(context.Context, int, int) ([]byte, error), store legacyNativeCheckpointIO) (*legacyNativeCheckpointTextWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length < 0 || uint64(length) > legacyNativeValueCheckpointPayloadLimit() || length > 0 && read == nil || store.Put == nil {
		return nil, ErrIntegrity
	}
	return &legacyNativeCheckpointTextWriter{store: store, read: read, length: length}, nil
}

func (work *legacyNativeCheckpointTextWriter) step(ctx context.Context) (bool, error) {
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
	if work.phase == legacyNativeCheckpointTextWrite {
		ref, err := legacyNativeCheckpointWrite(ctx, work.store, work.part)
		if err != nil {
			return work.fail(err)
		}
		work.pending, work.phase = ref, legacyNativeCheckpointTextIndex
		return false, nil
	}
	if work.phase == legacyNativeCheckpointTextIndex {
		if work.pending.Bytes != len(work.part) || work.pending.Bytes <= 0 || work.pending.Bytes > work.length-work.position {
			return work.fail(ErrIntegrity)
		}
		if err := work.tree.append(ctx, work.store, work.pending, uint64(work.pending.Bytes)); err != nil {
			return work.fail(err)
		}
		work.position += work.pending.Bytes
		work.part, work.pending, work.phase = nil, legacyNativeCheckpointRef{}, legacyNativeCheckpointTextRead
		return false, nil
	}
	if work.phase != legacyNativeCheckpointTextRead {
		return work.fail(ErrIntegrity)
	}
	if work.position < work.length {
		length := min(LegacyCanonicalEncodingBytes, work.length-work.position)
		body, err := work.read(ctx, work.position, length)
		if err != nil {
			return work.fail(err)
		}
		if len(body) != length {
			return work.fail(ErrIntegrity)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		work.part = make([]byte, length)
		copy(work.part, body)
		work.phase = legacyNativeCheckpointTextWrite
		return false, nil
	}
	tree, err := work.tree.finish(ctx, work.store)
	if err != nil {
		return work.fail(err)
	}
	if tree.Count != uint64(work.length) {
		return work.fail(ErrIntegrity)
	}
	work.result = legacyNativeCheckpointText{Bytes: work.length, Tree: tree}
	work.read = nil
	work.tree.close()
	work.ready = true
	return true, nil
}

func (work *legacyNativeCheckpointTextWriter) take(ctx context.Context) (legacyNativeCheckpointText, error) {
	if work == nil {
		return legacyNativeCheckpointText{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointText{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointText{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointText{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointTextWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.result.Tree.Ref.Hash)), 1)
	heap.allocation(int64(cap(work.part)), 1)
	heap.allocation(int64(len(work.pending.Hash)), 1)
	retained, err := work.tree.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointTextWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointTextWriter) close() {
	if work != nil {
		work.tree.close()
		failure := work.failure
		*work = legacyNativeCheckpointTextWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointScalarWriter struct {
	snapshot *legacyNativeScalarSnapshot
	text     *legacyNativeCheckpointTextWriter
	result   legacyNativeCheckpointScalar
	ready    bool
	closed   bool
	failure  error
}

func newLegacyNativeCheckpointScalarWriter(ctx context.Context, scalar *legacyNativeScalar, store legacyNativeCheckpointIO) (*legacyNativeCheckpointScalarWriter, error) {
	snapshot, err := scalar.checkpoint(ctx)
	if err != nil {
		return nil, err
	}
	text, err := newLegacyNativeCheckpointTextWriter(ctx, snapshot.state.Bytes, snapshot.read, store)
	if err != nil {
		snapshot.close()
		return nil, err
	}
	return &legacyNativeCheckpointScalarWriter{snapshot: snapshot, text: text}, nil
}

func (work *legacyNativeCheckpointScalarWriter) step(ctx context.Context) (bool, error) {
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
	ready, err := work.text.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ready {
		return false, nil
	}
	text, err := work.text.take(ctx)
	if err != nil {
		return work.fail(err)
	}
	work.result = legacyNativeCheckpointScalar{State: work.snapshot.state, Text: text}
	work.snapshot.close()
	work.snapshot, work.text = nil, nil
	work.ready = true
	return true, nil
}

func (work *legacyNativeCheckpointScalarWriter) take(ctx context.Context) (legacyNativeCheckpointScalar, error) {
	if work == nil {
		return legacyNativeCheckpointScalar{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointScalar{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointScalar{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointScalar{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointScalarWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	if work.snapshot != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.snapshot)))
	}
	retained, err := work.text.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	heap.allocation(int64(len(work.result.Text.Tree.Ref.Hash)), 1)
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointScalarWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointScalarWriter) close() {
	if work != nil {
		work.snapshot.close()
		work.text.close()
		failure := work.failure
		*work = legacyNativeCheckpointScalarWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointScalarRestore struct {
	saved       legacyNativeCheckpointScalar
	store       legacyNativeCheckpointIO
	cursor      *legacyNativeCheckpointTreeCursor
	parts       []ConversationManifestChunk
	partsHeap   docRangeHeaderHeapEstimate
	inner       *legacyNativeScalarRestore
	result      *legacyNativeScalar
	position    int
	loaded      int
	sourceBytes int
	budget      int64
	ready       bool
	closed      bool
	failure     error
}

func newLegacyNativeCheckpointScalarRestore(ctx context.Context, saved legacyNativeCheckpointScalar, store legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointScalarRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saved.State.validate(sourceBytes); err != nil {
		return nil, err
	}
	if heapBudget <= 0 || saved.Text.Bytes != saved.State.Bytes || !saved.Text.Tree.valid() || saved.Text.Tree.Count != uint64(saved.Text.Bytes) || (saved.Text.Bytes == 0) != (saved.Text.Tree == (legacyNativeCheckpointTree{})) || store.Read == nil {
		return nil, ErrIntegrity
	}
	count := 0
	if saved.Text.Bytes > 0 {
		count = 1 + (saved.Text.Bytes-1)/LegacyCanonicalEncodingBytes
	}
	var descriptorHeap, hashHeap, peak docRangeHeaderHeapEstimate
	descriptorHeap.allocation(int64(count), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	hashHeap.allocation(int64(len(contentHashPrefix)+64), 1)
	peak.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointScalarRestore{})))
	peak.include(descriptorHeap)
	peak.include(descriptorHeap)
	peak.include(docRangeHeaderHeapEstimate{bytes: int64(count) * 2 * hashHeap.bytes, err: hashHeap.err})
	peak.allocation(1, int64(unsafe.Sizeof(legacyNativeScalarRestore{})))
	peak.allocation(1, int64(unsafe.Sizeof(legacyNativeScalar{})))
	peak.allocation(1, 64)
	if saved.State.Present {
		peak.allocation(legacyNativeScalarAllocationBound(saved.State.Capacity), 1)
	}
	peak.allocation(4*LegacyCanonicalEncodingBytes, 1)
	if peak.err != nil {
		return nil, peak.err
	}
	if peak.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	cursor, err := newLegacyNativeCheckpointTreeCursor(saved.Text.Tree, store)
	if err != nil {
		return nil, err
	}
	work := &legacyNativeCheckpointScalarRestore{saved: saved, store: store, cursor: cursor, sourceBytes: sourceBytes, budget: heapBudget}
	work.parts = make([]ConversationManifestChunk, count)
	work.partsHeap = descriptorHeap
	return work, nil
}

func (work *legacyNativeCheckpointScalarRestore) step(ctx context.Context) (bool, error) {
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
	if work.cursor != nil {
		ref, count, ok, err := work.cursor.next(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ok {
			length := min(LegacyCanonicalEncodingBytes, work.saved.Text.Bytes-work.position)
			if work.loaded >= len(work.parts) || length <= 0 || count != uint64(length) || ref.Bytes != length {
				return work.fail(ErrIntegrity)
			}
			work.parts[work.loaded] = ConversationManifestChunk{Hash: ref.Hash, Bytes: int64(length)}
			work.partsHeap.allocation(int64(len(ref.Hash)), 1)
			work.loaded++
			work.position += length
			if err := work.checkBudget(ctx); err != nil {
				return work.fail(err)
			}
			return false, nil
		}
		if work.position != work.saved.Text.Bytes || work.loaded != len(work.parts) {
			return work.fail(ErrIntegrity)
		}
		work.cursor.close()
		work.cursor = nil
		heap, err := work.retainedBytes()
		if err != nil {
			return work.fail(err)
		}
		parts, store := work.parts, work.store
		prefix := LegacyByteSource{Bytes: work.saved.Text.Bytes, PartBytes: LegacyCanonicalEncodingBytes, Parts: work.parts}
		if prefix.Bytes > 0 {
			prefix.Read = func(current context.Context, offset, length int) ([]byte, error) {
				if offset < 0 || offset%LegacyCanonicalEncodingBytes != 0 || length <= 0 || length > LegacyCanonicalEncodingBytes {
					return nil, ErrIntegrity
				}
				ordinal := offset / LegacyCanonicalEncodingBytes
				if ordinal >= len(parts) || parts[ordinal].Bytes != int64(length) {
					return nil, ErrIntegrity
				}
				ref := legacyNativeCheckpointRef{Hash: parts[ordinal].Hash, Bytes: length}
				return legacyNativeCheckpointRead(current, store, ref)
			}
		}
		work.inner, err = newLegacyNativeScalarRestore(ctx, work.saved.State, prefix, work.sourceBytes, work.budget-heap)
		if err != nil {
			return work.fail(err)
		}
		work.parts, work.partsHeap = nil, docRangeHeaderHeapEstimate{}
		if err := work.checkBudget(ctx); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	ready, err := work.inner.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if ready {
		work.result, err = work.inner.take(ctx)
		if err != nil {
			return work.fail(err)
		}
		work.inner = nil
		work.ready = true
	}
	if err := work.checkBudget(ctx); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointScalarRestore) checkBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	heap, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if heap > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointScalarRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.saved.Text.Tree.Ref.Hash)), 1)
	heap.include(work.partsHeap)
	if work.cursor != nil {
		retained, err := work.cursor.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.inner != nil {
		retained, err := work.inner.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.result != nil {
		work.result.retained(&heap)
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointScalarRestore) take(ctx context.Context) (*legacyNativeScalar, error) {
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
	if work.closed || !work.ready || work.result == nil {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointScalarRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointScalarRestore) close() {
	if work != nil {
		if work.cursor != nil {
			work.cursor.close()
		}
		work.inner.close()
		failure := work.failure
		*work = legacyNativeCheckpointScalarRestore{closed: true, failure: failure}
	}
}
