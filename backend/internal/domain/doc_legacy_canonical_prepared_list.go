package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalPreparedListReader[Value any] struct {
	cursor *legacyNativeCheckpointTreeCursor
	store  legacyNativeCheckpointIO
	page   legacyNativeCheckpointListPage[Value]
	index  int
	bytes  int
	done   bool
}

func newLegacyCanonicalPreparedListReader[Value any](tree legacyNativeCheckpointTree, store legacyNativeCheckpointIO) (*legacyCanonicalPreparedListReader[Value], error) {
	cursor, err := newLegacyNativeCheckpointTreeCursor(tree, store)
	if err != nil {
		return nil, err
	}
	return &legacyCanonicalPreparedListReader[Value]{cursor: cursor, store: store}, nil
}

func (reader *legacyCanonicalPreparedListReader[Value]) next(ctx context.Context) (Value, bool, error) {
	var empty Value
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}
	if reader == nil || reader.cursor == nil {
		return empty, false, ErrIntegrity
	}
	if reader.done {
		return empty, false, nil
	}
	if reader.index == len(reader.page.Values) {
		clear(reader.page.Values)
		reader.page, reader.index, reader.bytes = legacyNativeCheckpointListPage[Value]{}, 0, 0
		ref, count, found, err := reader.cursor.next(ctx)
		if err != nil || !found {
			reader.done = err == nil
			return empty, false, err
		}
		if count == 0 || count > legacyNativeCheckpointPageItems {
			return empty, false, ErrIntegrity
		}
		if err := legacyNativeCheckpointReadJSON(ctx, reader.store, ref, &reader.page); err != nil {
			return empty, false, err
		}
		if reader.page.Version != legacyNativeCheckpointVersion || uint64(len(reader.page.Values)) != count {
			return empty, false, ErrIntegrity
		}
		reader.bytes = 4 * ref.Bytes
	}
	value := reader.page.Values[reader.index]
	reader.index++
	return value, true, nil
}

func (reader *legacyCanonicalPreparedListReader[Value]) retainedBytes() (int64, error) {
	if reader == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*reader)))
	heap.allocation(int64(cap(reader.page.Values)), int64(unsafe.Sizeof(*new(Value))))
	heap.allocation(int64(reader.bytes), 1)
	if reader.cursor != nil {
		retained, err := reader.cursor.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (reader *legacyCanonicalPreparedListReader[Value]) close() {
	if reader != nil {
		clear(reader.page.Values)
		if reader.cursor != nil {
			reader.cursor.close()
		}
		*reader = legacyCanonicalPreparedListReader[Value]{done: true}
	}
}
