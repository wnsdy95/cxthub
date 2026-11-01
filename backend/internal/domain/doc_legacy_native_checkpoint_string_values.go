package domain

import (
	"context"
	"strings"
	"unsafe"
)

type legacyNativeCheckpointStringValueRow struct {
	Capacity int                        `json:"capacity"`
	Text     legacyNativeCheckpointText `json:"text"`
}

func (row legacyNativeCheckpointStringValueRow) validate() error {
	maximum := legacyNativeValueCheckpointPayloadLimit()
	if row.Text.Bytes < 0 || uint64(row.Text.Bytes) > maximum || row.Capacity < row.Text.Bytes ||
		uint64(row.Capacity) > 2*maximum+64 || !row.Text.Tree.valid() ||
		row.Text.Tree.Count != uint64(row.Text.Bytes) ||
		(row.Text.Bytes == 0) != (row.Text.Tree == (legacyNativeCheckpointTree{})) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointStringValuesWorkspace() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(8*LegacyCanonicalEncodingBytes, 1)
	heap.allocation(2*(legacyNativeCheckpointTreeHeight+1)*legacyNativeCheckpointPageItems,
		int64(unsafe.Sizeof(legacyNativeCheckpointTree{}))+int64(len(contentHashPrefix)+64))
	heap.allocation(legacyNativeCheckpointPageItems, int64(unsafe.Sizeof(legacyNativeCheckpointStringValueRow{})))
	heap.allocation(1, 4096)
	return heap
}

func legacyNativeCheckpointStringValuesBudget(retained int64, additional docRangeHeaderHeapEstimate, budget int64) error {
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.include(additional)
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

type legacyNativeCheckpointStringValuesWriter struct {
	store      legacyNativeCheckpointIO
	values     []string
	capacities func(context.Context, int) (int, error)
	rows       legacyNativeCheckpointListWriter[legacyNativeCheckpointStringValueRow]
	text       *legacyNativeCheckpointTextWriter
	row        legacyNativeCheckpointStringValueRow
	result     legacyNativeCheckpointStrings
	position   int
	budget     int64
	pending    bool
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeCheckpointStringValuesWriter(ctx context.Context, values []string, capacities func(context.Context, int) (int, error), store legacyNativeCheckpointIO, heapBudget int64) (*legacyNativeCheckpointStringValuesWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := legacyNativeCheckpointStrings{Nil: values == nil, Length: len(values), Capacity: cap(values)}
	if err := result.validate(false); err != nil {
		return nil, err
	}
	if store.Put == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointStringValuesWriter{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointListWriter[legacyNativeCheckpointStringValueRow]{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeBuilder{})))
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, heapBudget); err != nil {
		return nil, err
	}
	return &legacyNativeCheckpointStringValuesWriter{
		store: store, values: values[:cap(values)], capacities: capacities, result: result, budget: heapBudget,
	}, nil
}

func (work *legacyNativeCheckpointStringValuesWriter) step(ctx context.Context) (bool, error) {
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
	if work.text != nil {
		if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
			return work.fail(err)
		}
		ready, err := work.text.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.row.Text, err = work.text.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.text = nil
			work.pending = true
		}
		return work.progress(ctx)
	}
	if work.pending {
		if err := work.row.validate(); err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
			return work.fail(err)
		}
		if err := work.rows.append(ctx, work.store, work.row); err != nil {
			return work.fail(err)
		}
		work.position++
		work.row, work.pending = legacyNativeCheckpointStringValueRow{}, false
		return work.progress(ctx)
	}
	if work.position < len(work.values) {
		length := len(work.values[work.position])
		capacity := length
		if work.capacities != nil {
			var err error
			capacity, err = work.capacities(ctx, work.position)
			if err != nil {
				return work.fail(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		maximum := legacyNativeValueCheckpointPayloadLimit()
		if uint64(length) > maximum || capacity < length || uint64(capacity) > 2*maximum+64 {
			return work.fail(ErrIntegrity)
		}
		if length <= LegacyCanonicalEncodingBytes {
			if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
				return work.fail(err)
			}
			var tree legacyNativeCheckpointTree
			if length > 0 {
				ref, err := legacyNativeCheckpointWrite(ctx, work.store, []byte(work.values[work.position]))
				if err != nil {
					return work.fail(err)
				}
				tree = legacyNativeCheckpointTree{Ref: ref, Count: uint64(length)}
			}
			work.row = legacyNativeCheckpointStringValueRow{Capacity: capacity, Text: legacyNativeCheckpointText{Bytes: length, Tree: tree}}
			work.pending = true
			return work.progress(ctx)
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTextWriter{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeBuilder{})))
		planned.allocation(1, 64)
		if err := work.checkBudget(planned); err != nil {
			return work.fail(err)
		}
		var err error
		work.text, err = newLegacyNativeCheckpointTextWriter(ctx, length, work.read, work.store)
		if err != nil {
			return work.fail(err)
		}
		work.row.Capacity = capacity
		return work.progress(ctx)
	}
	if work.position != work.result.Capacity {
		return work.fail(ErrIntegrity)
	}
	if work.result.Capacity > 0 {
		if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
			return work.fail(err)
		}
		var err error
		work.result.Items, err = work.rows.finish(ctx, work.store)
		if err != nil {
			return work.fail(err)
		}
	}
	if err := work.result.validate(true); err != nil {
		return work.fail(err)
	}
	work.rows.close()
	work.values, work.capacities = nil, nil
	work.ready = true
	return work.progress(ctx)
}

func (work *legacyNativeCheckpointStringValuesWriter) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil || work.closed || work.position < 0 || work.position >= len(work.values) ||
		offset < 0 || offset > len(work.values[work.position]) || length <= 0 ||
		length > LegacyCanonicalEncodingBytes || length > len(work.values[work.position])-offset {
		return nil, ErrIntegrity
	}
	body := []byte(work.values[work.position][offset : offset+length])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func (work *legacyNativeCheckpointStringValuesWriter) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(docRangeHeaderHeapEstimate{}); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointStringValuesWriter) checkBudget(additional docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointStringValuesBudget(retained, additional, work.budget)
}

func (work *legacyNativeCheckpointStringValuesWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.row.Text.Tree.Ref.Hash)), 1)
	heap.allocation(int64(len(work.result.Items.Ref.Hash)), 1)
	retained, err := work.rows.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.text != nil {
		retained, err := work.text.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		heap.allocation(1, 64)
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointStringValuesWriter) take(ctx context.Context) (legacyNativeCheckpointStrings, error) {
	if work == nil {
		return legacyNativeCheckpointStrings{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointStrings{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointStrings{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointStrings{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointStringValuesWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointStringValuesWriter) close() {
	if work != nil {
		work.rows.close()
		work.text.close()
		failure := work.failure
		*work = legacyNativeCheckpointStringValuesWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointStringValuesRestore struct {
	saved        legacyNativeCheckpointStrings
	store        legacyNativeCheckpointIO
	rows         *legacyNativeCheckpointListReader[legacyNativeCheckpointStringValueRow]
	scalar       *legacyNativeCheckpointScalarRestore
	row          legacyNativeCheckpointStringValueRow
	result       legacyNativeCheckpointStringValues
	materialized *legacyNativeScalar
	exact        []byte
	copied       int
	position     int
	sourceBytes  int
	budget       int64
	ready        bool
	closed       bool
	failure      error
}

func newLegacyNativeCheckpointStringValuesRestore(ctx context.Context, saved legacyNativeCheckpointStrings, store legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointStringValuesRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saved.validate(true); err != nil {
		return nil, err
	}
	if sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes || store.Read == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointStringValuesRestore{})))
	planned.allocation(int64(len(saved.Items.Ref.Hash)), 1)
	planned.allocation(int64(saved.Capacity), int64(unsafe.Sizeof(string(""))))
	planned.allocation(int64(saved.Capacity), int64(unsafe.Sizeof(int(0))))
	if saved.Capacity > 0 {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointListReader[legacyNativeCheckpointStringValueRow]{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeCursor{})))
		planned.allocation(int64(len(saved.Items.Ref.Hash)), 1)
	}
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, heapBudget); err != nil {
		return nil, err
	}
	saved.Items.Ref.Hash = ContentHash(strings.Clone(string(saved.Items.Ref.Hash)))
	work := &legacyNativeCheckpointStringValuesRestore{saved: saved, store: store, sourceBytes: sourceBytes, budget: heapBudget}
	if !saved.Nil {
		work.result.Values = make([]string, saved.Capacity)
		work.result.Capacities = make([]int, saved.Capacity)
	}
	if saved.Capacity > 0 {
		var err error
		work.rows, err = newLegacyNativeCheckpointListReader[legacyNativeCheckpointStringValueRow](saved.Items, store)
		if err != nil {
			work.close()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointStringValuesRestore) step(ctx context.Context) (bool, error) {
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
	if work.materialized != nil {
		if work.exact == nil {
			var planned docRangeHeaderHeapEstimate
			planned.allocation(int64(work.row.Capacity), 1)
			if err := work.checkBudget(planned); err != nil {
				return work.fail(err)
			}
			work.exact = make([]byte, work.row.Capacity)
			return work.progress(ctx)
		}
		length := min(len(work.exact)-work.copied, LegacyCanonicalEncodingBytes)
		copy(work.exact[work.copied:work.copied+length], work.materialized.text.String()[work.copied:work.copied+length])
		work.copied += length
		if work.copied == len(work.exact) {
			work.result.Values[work.position] = unsafe.String(unsafe.SliceData(work.exact), len(work.exact))
			work.result.Capacities[work.position] = cap(work.exact)
			work.result.Payload.allocation(int64(cap(work.exact)), 1)
			if work.result.Payload.err != nil {
				return work.fail(work.result.Payload.err)
			}
			work.position++
			work.row = legacyNativeCheckpointStringValueRow{}
			work.exact, work.materialized, work.copied = nil, nil, 0
		}
		return work.progress(ctx)
	}
	if work.scalar != nil {
		if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
			return work.fail(err)
		}
		ready, err := work.scalar.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			value, err := work.scalar.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.scalar = nil
			if value == nil || value.text == nil || value.previous != nil || value.position != 0 || value.end != 0 ||
				value.maximum != int(legacyNativeValueCheckpointPayloadLimit()) || value.buffered != 0 || value.expanded ||
				value.text.Len() != work.row.Text.Bytes ||
				value.text.Cap() != work.row.Capacity && work.row.Capacity != work.row.Text.Bytes ||
				work.position >= work.saved.Capacity {
				return work.fail(ErrIntegrity)
			}
			if work.row.Capacity == work.row.Text.Bytes && value.text.Cap() != work.row.Capacity {
				work.materialized = value
				return work.progress(ctx)
			}
			payload := work.result.Payload
			payload.allocation(int64(value.text.Cap()), 1)
			if payload.err != nil {
				return work.fail(payload.err)
			}
			work.result.Values[work.position] = value.text.String()
			work.result.Capacities[work.position] = value.text.Cap()
			work.result.Payload = payload
			work.position++
			work.row = legacyNativeCheckpointStringValueRow{}
		}
		return work.progress(ctx)
	}
	if work.rows == nil {
		if work.saved.Capacity != 0 {
			return work.fail(ErrIntegrity)
		}
		work.ready = true
		return work.progress(ctx)
	}
	if err := work.checkBudget(legacyNativeCheckpointStringValuesWorkspace()); err != nil {
		return work.fail(err)
	}
	row, ok, err := work.rows.next(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ok {
		if work.position != work.saved.Capacity {
			return work.fail(ErrIntegrity)
		}
		work.rows.close()
		work.rows = nil
		work.result.Values = work.result.Values[:work.saved.Length:work.saved.Capacity]
		work.ready = true
		return work.progress(ctx)
	}
	if work.position >= work.saved.Capacity {
		return work.fail(ErrIntegrity)
	}
	if err := row.validate(); err != nil {
		return work.fail(err)
	}
	work.row = row
	retained, err := work.retainedBytes()
	if err != nil {
		return work.fail(err)
	}
	scratch := legacyNativeCheckpointStringValuesWorkspace()
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
			Bytes: row.Text.Bytes, Capacity: row.Capacity, Present: true,
		},
		Text: row.Text,
	}
	work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, saved, work.store, work.sourceBytes, remaining)
	if err != nil {
		return work.fail(err)
	}
	return work.progress(ctx)
}

func (work *legacyNativeCheckpointStringValuesRestore) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(docRangeHeaderHeapEstimate{}); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointStringValuesRestore) checkBudget(additional docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointStringValuesBudget(retained, additional, work.budget)
}

func (work *legacyNativeCheckpointStringValuesRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.saved.Items.Ref.Hash)), 1)
	heap.allocation(int64(len(work.row.Text.Tree.Ref.Hash)), 1)
	heap.allocation(int64(cap(work.result.Values)), int64(unsafe.Sizeof(string(""))))
	heap.allocation(int64(cap(work.result.Capacities)), int64(unsafe.Sizeof(int(0))))
	heap.include(work.result.Payload)
	heap.allocation(int64(cap(work.exact)), 1)
	if work.materialized != nil {
		work.materialized.retained(&heap)
	}
	if work.rows != nil {
		retained, err := work.rows.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointStringValuesRestore) take(ctx context.Context) (legacyNativeCheckpointStringValues, error) {
	if work == nil {
		return legacyNativeCheckpointStringValues{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointStringValues{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointStringValues{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointStringValues{}, ErrIntegrity
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointStringValuesRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointStringValuesRestore) close() {
	if work != nil {
		work.rows.close()
		work.scalar.close()
		failure := work.failure
		*work = legacyNativeCheckpointStringValuesRestore{closed: true, failure: failure}
	}
}
