package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"unsafe"
)

func (ref legacyNativeCheckpointRef) valid() bool {
	return ValidateContentHash(ref.Hash) == nil && ref.Bytes > 0 && ref.Bytes <= LegacyCanonicalEncodingBytes
}

func (tree legacyNativeCheckpointTree) valid() bool {
	if tree == (legacyNativeCheckpointTree{}) {
		return true
	}
	return tree.Ref.valid() && tree.Count > 0 && tree.Count <= legacyNativeValueCheckpointPayloadLimit() && tree.Height <= legacyNativeCheckpointTreeHeight
}

func legacyNativeCheckpointWrite(ctx context.Context, store legacyNativeCheckpointIO, body []byte) (legacyNativeCheckpointRef, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if store.Put == nil || len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	owned := bytes.Clone(body)
	hash, err := HashContentContext(ctx, owned)
	if err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if err := store.Put(ctx, hash, owned); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	return legacyNativeCheckpointRef{Hash: hash, Bytes: len(owned)}, nil
}

func legacyNativeCheckpointRead(ctx context.Context, store legacyNativeCheckpointIO, ref legacyNativeCheckpointRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || store.Read == nil {
		return nil, ErrIntegrity
	}
	body, err := store.Read(ctx, ref.Hash, ref.Bytes)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) != ref.Bytes {
		return nil, ErrPreparedDocRangeObject
	}
	owned := bytes.Clone(body)
	hash, err := HashContentContext(ctx, owned)
	if err != nil {
		return nil, err
	}
	if hash != ref.Hash {
		return nil, ErrPreparedDocRangeObject
	}
	return owned, nil
}

func legacyNativeCheckpointWriteJSON(ctx context.Context, store legacyNativeCheckpointIO, value any) (legacyNativeCheckpointRef, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	return legacyNativeCheckpointWrite(ctx, store, body)
}

func legacyNativeCheckpointReadJSON(ctx context.Context, store legacyNativeCheckpointIO, ref legacyNativeCheckpointRef, target any) error {
	body, err := legacyNativeCheckpointRead(ctx, store, ref)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrIntegrity
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrIntegrity
	}
	canonical, err := json.Marshal(target)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil || !bytes.Equal(body, canonical) {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeCheckpointBranch struct {
	Version  uint8                        `json:"v"`
	Children []legacyNativeCheckpointTree `json:"e"`
}

func (branch *legacyNativeCheckpointBranch) UnmarshalJSON(body []byte) error {
	return legacyNativeCheckpointDecodePage(body, &branch.Version, &branch.Children)
}

func legacyNativeCheckpointDecodePage[Value any](body []byte, version *uint8, values *[]Value) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	for _, want := range []any{json.Delim('{'), "v"} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return ErrIntegrity
		}
	}
	if err := decoder.Decode(version); err != nil {
		return ErrIntegrity
	}
	for _, want := range []any{"e", json.Delim('[')} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return ErrIntegrity
		}
	}
	decoded := make([]Value, 0, legacyNativeCheckpointPageItems)
	for decoder.More() {
		if len(decoded) == legacyNativeCheckpointPageItems {
			return ErrIntegrity
		}
		var value Value
		if err := decoder.Decode(&value); err != nil {
			return ErrIntegrity
		}
		decoded = append(decoded, value)
	}
	for _, want := range []any{json.Delim(']'), json.Delim('}')} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return ErrIntegrity
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	*values = decoded
	return nil
}

func legacyNativeCheckpointChildren(ctx context.Context, tree legacyNativeCheckpointTree, store legacyNativeCheckpointIO) ([]legacyNativeCheckpointTree, error) {
	if !tree.valid() || tree.Height == 0 {
		return nil, ErrIntegrity
	}
	var branch legacyNativeCheckpointBranch
	if err := legacyNativeCheckpointReadJSON(ctx, store, tree.Ref, &branch); err != nil {
		return nil, err
	}
	if branch.Version != legacyNativeCheckpointVersion || len(branch.Children) == 0 || len(branch.Children) > legacyNativeCheckpointPageItems {
		return nil, ErrIntegrity
	}
	var count uint64
	for _, child := range branch.Children {
		if !child.valid() || child.Count == 0 || child.Height+1 != tree.Height || child.Count > tree.Count-count {
			return nil, ErrIntegrity
		}
		count += child.Count
	}
	if count != tree.Count {
		return nil, ErrIntegrity
	}
	return branch.Children, nil
}

type legacyNativeCheckpointTreeBuilder struct {
	levels  [legacyNativeCheckpointTreeHeight + 1][]legacyNativeCheckpointTree
	count   uint64
	root    legacyNativeCheckpointTree
	done    bool
	closed  bool
	failure error
}

func (builder *legacyNativeCheckpointTreeBuilder) append(ctx context.Context, store legacyNativeCheckpointIO, ref legacyNativeCheckpointRef, count uint64) (failure error) {
	defer func() {
		if builder != nil && failure != nil && builder.failure == nil {
			builder.close()
			builder.failure = failure
		}
	}()
	if builder != nil && builder.failure != nil {
		return builder.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if builder == nil || builder.closed || builder.done || !ref.valid() || count == 0 || count > legacyNativeValueCheckpointPayloadLimit()-builder.count {
		return ErrIntegrity
	}
	if err := builder.push(ctx, store, legacyNativeCheckpointTree{Ref: ref, Count: count}, 0); err != nil {
		return err
	}
	builder.count += count
	return nil
}

func (builder *legacyNativeCheckpointTreeBuilder) push(ctx context.Context, store legacyNativeCheckpointIO, tree legacyNativeCheckpointTree, level int) error {
	if level < 0 || level >= len(builder.levels) || tree.Height != uint8(level) {
		return ErrIntegrity
	}
	if builder.levels[level] == nil {
		builder.levels[level] = make([]legacyNativeCheckpointTree, 0, legacyNativeCheckpointPageItems)
	}
	builder.levels[level] = append(builder.levels[level], tree)
	if len(builder.levels[level]) == legacyNativeCheckpointPageItems {
		return builder.flush(ctx, store, level)
	}
	return nil
}

func (builder *legacyNativeCheckpointTreeBuilder) flush(ctx context.Context, store legacyNativeCheckpointIO, level int) error {
	if level < 0 || level >= legacyNativeCheckpointTreeHeight || len(builder.levels[level]) == 0 {
		return ErrIntegrity
	}
	children := builder.levels[level]
	var count uint64
	for _, child := range children {
		if !child.valid() || child.Count > legacyNativeValueCheckpointPayloadLimit()-count {
			return ErrIntegrity
		}
		count += child.Count
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, store, legacyNativeCheckpointBranch{Version: legacyNativeCheckpointVersion, Children: children})
	if err != nil {
		return err
	}
	clear(builder.levels[level])
	builder.levels[level] = builder.levels[level][:0]
	return builder.push(ctx, store, legacyNativeCheckpointTree{Ref: ref, Height: uint8(level + 1), Count: count}, level+1)
}

func (builder *legacyNativeCheckpointTreeBuilder) finish(ctx context.Context, store legacyNativeCheckpointIO) (result legacyNativeCheckpointTree, failure error) {
	defer func() {
		if builder != nil && failure != nil && builder.failure == nil {
			builder.close()
			builder.failure = failure
		}
	}()
	if builder != nil && builder.failure != nil {
		return legacyNativeCheckpointTree{}, builder.failure
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointTree{}, err
	}
	if builder == nil || builder.closed {
		return legacyNativeCheckpointTree{}, ErrIntegrity
	}
	if builder.done {
		return builder.root, nil
	}
	for {
		first, entries := -1, 0
		for level, values := range builder.levels {
			if len(values) != 0 && first < 0 {
				first = level
			}
			entries += len(values)
		}
		if entries <= 1 {
			if first >= 0 {
				builder.root = builder.levels[first][0]
			}
			if builder.root.Count != builder.count {
				return legacyNativeCheckpointTree{}, ErrIntegrity
			}
			builder.done = true
			return builder.root, nil
		}
		if err := builder.flush(ctx, store, first); err != nil {
			return legacyNativeCheckpointTree{}, err
		}
	}
}

func legacyNativeCheckpointTreeHeap(estimate *docRangeHeaderHeapEstimate, trees []legacyNativeCheckpointTree) {
	estimate.allocation(int64(cap(trees)), int64(unsafe.Sizeof(legacyNativeCheckpointTree{})))
	for _, tree := range trees {
		estimate.allocation(int64(len(tree.Ref.Hash)), 1)
	}
}

func (builder *legacyNativeCheckpointTreeBuilder) retainedBytes() (int64, error) {
	if builder == nil || builder.closed {
		return 0, nil
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*builder)))
	for _, values := range builder.levels {
		legacyNativeCheckpointTreeHeap(&estimate, values)
	}
	return estimate.bytes, estimate.err
}

func (builder *legacyNativeCheckpointTreeBuilder) close() {
	if builder != nil {
		failure := builder.failure
		*builder = legacyNativeCheckpointTreeBuilder{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointTreeFrame struct {
	children []legacyNativeCheckpointTree
	next     int
}

type legacyNativeCheckpointTreeCursor struct {
	store   legacyNativeCheckpointIO
	pending legacyNativeCheckpointTree
	frames  []legacyNativeCheckpointTreeFrame
	want    uint64
	read    uint64
	closed  bool
	failure error
}

func newLegacyNativeCheckpointTreeCursor(tree legacyNativeCheckpointTree, store legacyNativeCheckpointIO) (*legacyNativeCheckpointTreeCursor, error) {
	if !tree.valid() || tree.Count > 0 && store.Read == nil {
		return nil, ErrIntegrity
	}
	return &legacyNativeCheckpointTreeCursor{store: store, pending: tree, want: tree.Count}, nil
}

func (cursor *legacyNativeCheckpointTreeCursor) next(ctx context.Context) (legacyNativeCheckpointRef, uint64, bool, error) {
	fail := func(err error) (legacyNativeCheckpointRef, uint64, bool, error) {
		cursor.close()
		cursor.failure = err
		return legacyNativeCheckpointRef{}, 0, false, err
	}
	if cursor == nil {
		return legacyNativeCheckpointRef{}, 0, false, ErrIntegrity
	}
	if cursor.failure != nil {
		return legacyNativeCheckpointRef{}, 0, false, cursor.failure
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if cursor.closed {
		return legacyNativeCheckpointRef{}, 0, false, ErrIntegrity
	}
	for {
		if cursor.pending.Count != 0 {
			tree := cursor.pending
			cursor.pending = legacyNativeCheckpointTree{}
			if tree.Height == 0 {
				if tree.Count > cursor.want-cursor.read {
					return fail(ErrIntegrity)
				}
				cursor.read += tree.Count
				return tree.Ref, tree.Count, true, nil
			}
			children, err := legacyNativeCheckpointChildren(ctx, tree, cursor.store)
			if err != nil {
				return fail(err)
			}
			if len(cursor.frames) >= legacyNativeCheckpointTreeHeight {
				return fail(ErrIntegrity)
			}
			cursor.frames = append(cursor.frames, legacyNativeCheckpointTreeFrame{children: children})
		}
		for len(cursor.frames) > 0 {
			frame := &cursor.frames[len(cursor.frames)-1]
			if frame.next < len(frame.children) {
				cursor.pending = frame.children[frame.next]
				frame.next++
				break
			}
			*frame = legacyNativeCheckpointTreeFrame{}
			cursor.frames = cursor.frames[:len(cursor.frames)-1]
		}
		if cursor.pending.Count == 0 && len(cursor.frames) == 0 {
			if cursor.read != cursor.want {
				return fail(ErrIntegrity)
			}
			return legacyNativeCheckpointRef{}, 0, false, nil
		}
	}
}

func (cursor *legacyNativeCheckpointTreeCursor) retainedBytes() (int64, error) {
	if cursor == nil || cursor.closed {
		return 0, nil
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*cursor)))
	estimate.allocation(int64(cap(cursor.frames)), int64(unsafe.Sizeof(legacyNativeCheckpointTreeFrame{})))
	for _, frame := range cursor.frames {
		legacyNativeCheckpointTreeHeap(&estimate, frame.children)
	}
	estimate.allocation(int64(len(cursor.pending.Ref.Hash)), 1)
	return estimate.bytes, estimate.err
}

func (cursor *legacyNativeCheckpointTreeCursor) close() {
	if cursor != nil {
		failure := cursor.failure
		*cursor = legacyNativeCheckpointTreeCursor{closed: true, failure: failure}
	}
}

func legacyNativeCheckpointLeafAt(ctx context.Context, store legacyNativeCheckpointIO, tree legacyNativeCheckpointTree, offset uint64) (legacyNativeCheckpointRef, uint64, error) {
	if !tree.valid() || offset >= tree.Count {
		return legacyNativeCheckpointRef{}, 0, ErrIntegrity
	}
	var start uint64
	for tree.Height > 0 {
		children, err := legacyNativeCheckpointChildren(ctx, tree, store)
		if err != nil {
			return legacyNativeCheckpointRef{}, 0, err
		}
		found := false
		for _, child := range children {
			if offset-start < child.Count {
				tree, found = child, true
				break
			}
			start += child.Count
		}
		if !found {
			return legacyNativeCheckpointRef{}, 0, ErrIntegrity
		}
	}
	return tree.Ref, start, ctx.Err()
}

type legacyNativeCheckpointListPage[Value any] struct {
	Version uint8   `json:"v"`
	Values  []Value `json:"e"`
}

func (page *legacyNativeCheckpointListPage[Value]) UnmarshalJSON(body []byte) error {
	return legacyNativeCheckpointDecodePage(body, &page.Version, &page.Values)
}

type legacyNativeCheckpointListWriter[Value any] struct {
	values  []Value
	tree    legacyNativeCheckpointTreeBuilder
	bytes   int64
	done    bool
	closed  bool
	failure error
}

func (writer *legacyNativeCheckpointListWriter[Value]) append(ctx context.Context, store legacyNativeCheckpointIO, value Value) (failure error) {
	defer func() {
		if writer != nil && failure != nil && writer.failure == nil {
			writer.close()
			writer.failure = failure
		}
	}()
	if writer != nil && writer.failure != nil {
		return writer.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer == nil || writer.done || writer.closed || unsafe.Sizeof(value) > LegacyCanonicalEncodingBytes/legacyNativeCheckpointPageItems {
		return ErrIntegrity
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > LegacyCanonicalEncodingBytes {
		return ErrIntegrity
	}
	if writer.values == nil {
		writer.values = make([]Value, 0, legacyNativeCheckpointPageItems)
	}
	writer.values = append(writer.values, value)
	writer.bytes += int64(4*len(body) + 1024)
	if len(writer.values) == legacyNativeCheckpointPageItems {
		return writer.flush(ctx, store)
	}
	return nil
}

func (writer *legacyNativeCheckpointListWriter[Value]) flush(ctx context.Context, store legacyNativeCheckpointIO) error {
	if len(writer.values) == 0 {
		return nil
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, store, legacyNativeCheckpointListPage[Value]{Version: legacyNativeCheckpointVersion, Values: writer.values})
	if err != nil {
		return err
	}
	if err := writer.tree.append(ctx, store, ref, uint64(len(writer.values))); err != nil {
		return err
	}
	clear(writer.values)
	writer.values, writer.bytes = writer.values[:0], 0
	return nil
}

func (writer *legacyNativeCheckpointListWriter[Value]) finish(ctx context.Context, store legacyNativeCheckpointIO) (result legacyNativeCheckpointTree, failure error) {
	defer func() {
		if writer != nil && failure != nil && writer.failure == nil {
			writer.close()
			writer.failure = failure
		}
	}()
	if writer != nil && writer.failure != nil {
		return legacyNativeCheckpointTree{}, writer.failure
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointTree{}, err
	}
	if writer == nil || writer.closed {
		return legacyNativeCheckpointTree{}, ErrIntegrity
	}
	if !writer.done {
		if err := writer.flush(ctx, store); err != nil {
			return legacyNativeCheckpointTree{}, err
		}
		writer.done = true
	}
	return writer.tree.finish(ctx, store)
}

func (writer *legacyNativeCheckpointListWriter[Value]) retainedBytes() (int64, error) {
	if writer == nil || writer.closed {
		return 0, nil
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*writer)))
	estimate.allocation(int64(cap(writer.values)), int64(unsafe.Sizeof(*new(Value))))
	estimate.include(docRangeHeaderHeapEstimate{bytes: writer.bytes})
	bytes, err := writer.tree.retainedBytes()
	estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	return estimate.bytes, estimate.err
}

func (writer *legacyNativeCheckpointListWriter[Value]) close() {
	if writer != nil {
		writer.tree.close()
		failure := writer.failure
		*writer = legacyNativeCheckpointListWriter[Value]{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointListReader[Value any] struct {
	store   legacyNativeCheckpointIO
	cursor  *legacyNativeCheckpointTreeCursor
	values  []Value
	index   int
	bytes   int64
	closed  bool
	failure error
}

func newLegacyNativeCheckpointListReader[Value any](tree legacyNativeCheckpointTree, store legacyNativeCheckpointIO) (*legacyNativeCheckpointListReader[Value], error) {
	cursor, err := newLegacyNativeCheckpointTreeCursor(tree, store)
	if err != nil {
		return nil, err
	}
	return &legacyNativeCheckpointListReader[Value]{store: store, cursor: cursor}, nil
}

func (reader *legacyNativeCheckpointListReader[Value]) next(ctx context.Context) (Value, bool, error) {
	var zero Value
	if reader == nil {
		return zero, false, ErrIntegrity
	}
	fail := func(err error) (Value, bool, error) {
		reader.close()
		reader.failure = err
		return zero, false, err
	}
	if reader.failure != nil {
		return zero, false, reader.failure
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if reader.closed {
		return zero, false, ErrIntegrity
	}
	if reader.index == len(reader.values) {
		ref, count, ok, err := reader.cursor.next(ctx)
		if err != nil {
			return fail(err)
		}
		if !ok {
			return zero, false, nil
		}
		if count == 0 || count > legacyNativeCheckpointPageItems {
			return fail(ErrIntegrity)
		}
		var page legacyNativeCheckpointListPage[Value]
		if err := legacyNativeCheckpointReadJSON(ctx, reader.store, ref, &page); err != nil {
			return fail(err)
		}
		if page.Version != legacyNativeCheckpointVersion || uint64(len(page.Values)) != count {
			return fail(ErrIntegrity)
		}
		reader.values, reader.index, reader.bytes = page.Values, 0, int64(4*ref.Bytes+1024)
	}
	value := reader.values[reader.index]
	reader.index++
	return value, true, nil
}

func (reader *legacyNativeCheckpointListReader[Value]) retainedBytes() (int64, error) {
	if reader == nil || reader.closed {
		return 0, nil
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*reader)))
	estimate.allocation(int64(cap(reader.values)), int64(unsafe.Sizeof(*new(Value))))
	estimate.include(docRangeHeaderHeapEstimate{bytes: reader.bytes})
	bytes, err := reader.cursor.retainedBytes()
	estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	return estimate.bytes, estimate.err
}

func (reader *legacyNativeCheckpointListReader[Value]) close() {
	if reader != nil {
		reader.cursor.close()
		failure := reader.failure
		*reader = legacyNativeCheckpointListReader[Value]{closed: true, failure: failure}
	}
}
