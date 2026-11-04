package domain

import (
	"context"
	"encoding/binary"
	"unsafe"
)

const legacyNativeMapCheckpointEntryBytes = 2 * legacyNativeValueCheckpointBytes
const legacyNativeArrayCheckpointEntryBytes = 8 + legacyNativeValueCheckpointBytes

type legacyNativeMapCheckpointEntry struct {
	Key   legacyNativeValueCheckpoint
	Value legacyNativeValueCheckpoint
}

func (entry legacyNativeMapCheckpointEntry) encode(ctx context.Context, bounds legacyNativeValueCheckpointBounds) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.Key.Kind != legacyNativeCheckpointString {
		return nil, ErrIntegrity
	}
	key, err := entry.Key.encode(ctx, bounds)
	if err != nil {
		return nil, err
	}
	value, err := entry.Value.encode(ctx, bounds)
	if err != nil {
		return nil, err
	}
	body := make([]byte, legacyNativeMapCheckpointEntryBytes)
	copy(body, key)
	copy(body[legacyNativeValueCheckpointBytes:], value)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func decodeLegacyNativeMapCheckpointEntry(ctx context.Context, body []byte, bounds legacyNativeValueCheckpointBounds) (legacyNativeMapCheckpointEntry, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeMapCheckpointEntry{}, err
	}
	if len(body) != legacyNativeMapCheckpointEntryBytes {
		return legacyNativeMapCheckpointEntry{}, ErrIntegrity
	}
	key, err := decodeLegacyNativeValueCheckpoint(ctx, body[:legacyNativeValueCheckpointBytes], bounds)
	if err != nil {
		return legacyNativeMapCheckpointEntry{}, err
	}
	if key.Kind != legacyNativeCheckpointString {
		return legacyNativeMapCheckpointEntry{}, ErrIntegrity
	}
	value, err := decodeLegacyNativeValueCheckpoint(ctx, body[legacyNativeValueCheckpointBytes:], bounds)
	if err != nil {
		return legacyNativeMapCheckpointEntry{}, err
	}
	return legacyNativeMapCheckpointEntry{Key: key, Value: value}, nil
}

type legacyNativeArrayCheckpointEntry struct {
	Position uint64
	Value    legacyNativeValueCheckpoint
}

func (entry legacyNativeArrayCheckpointEntry) encode(ctx context.Context, bounds legacyNativeValueCheckpointBounds, length uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length > bounds.Nodes || entry.Position >= length {
		return nil, ErrIntegrity
	}
	value, err := entry.Value.encode(ctx, bounds)
	if err != nil {
		return nil, err
	}
	body := make([]byte, legacyNativeArrayCheckpointEntryBytes)
	binary.LittleEndian.PutUint64(body[:8], entry.Position)
	copy(body[8:], value)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func decodeLegacyNativeArrayCheckpointEntry(ctx context.Context, body []byte, bounds legacyNativeValueCheckpointBounds, length uint64) (legacyNativeArrayCheckpointEntry, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeArrayCheckpointEntry{}, err
	}
	if len(body) != legacyNativeArrayCheckpointEntryBytes {
		return legacyNativeArrayCheckpointEntry{}, ErrIntegrity
	}
	position := binary.LittleEndian.Uint64(body[:8])
	if length > bounds.Nodes || position >= length {
		return legacyNativeArrayCheckpointEntry{}, ErrIntegrity
	}
	value, err := decodeLegacyNativeValueCheckpoint(ctx, body[8:], bounds)
	if err != nil {
		return legacyNativeArrayCheckpointEntry{}, err
	}
	return legacyNativeArrayCheckpointEntry{Position: position, Value: value}, nil
}

type legacyNativeMapEntrySnapshot struct {
	key    *legacyNativeValueSnapshot
	value  *legacyNativeValueSnapshot
	closed bool
}

func (entry legacyNativeAnyMapCheckpointEntry) checkpoint(ctx context.Context, keyOffset, valueReference uint64, bounds legacyNativeValueCheckpointBounds) (*legacyNativeMapEntrySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.KeyCapacity < len(entry.Key) || entry.ValueBytes < 0 || entry.ValueExcess < 0 || entry.ValueExcess > entry.ValueBytes {
		return nil, ErrIntegrity
	}
	value := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
	if text, ok := entry.Value.(string); ok {
		extra := entry.ValueExcess / 2
		if entry.ValueExcess%2 != 0 || extra > int64(int(^uint(0)>>1)-len(text)) {
			return nil, ErrIntegrity
		}
		value.capacity = len(text) + int(extra)
	}
	key, err := (legacyNativeAnyValue{value: entry.Key, capacity: entry.KeyCapacity}).checkpoint(ctx, keyOffset, bounds)
	if err != nil {
		return nil, err
	}
	child, err := value.checkpoint(ctx, valueReference, bounds)
	if err != nil {
		key.close()
		return nil, err
	}
	return &legacyNativeMapEntrySnapshot{key: key, value: child}, nil
}

func (snapshot *legacyNativeMapEntrySnapshot) encode(ctx context.Context, bounds legacyNativeValueCheckpointBounds) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.closed || snapshot.key == nil || snapshot.value == nil || snapshot.key.closed || snapshot.value.closed {
		return nil, ErrIntegrity
	}
	return (legacyNativeMapCheckpointEntry{Key: snapshot.key.state, Value: snapshot.value.state}).encode(ctx, bounds)
}

func (snapshot *legacyNativeMapEntrySnapshot) retainedBytes() (int64, error) {
	if snapshot == nil || snapshot.closed {
		return 0, nil
	}
	if snapshot.key == nil || snapshot.value == nil || snapshot.key.closed || snapshot.value.closed {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*snapshot)))
	for _, child := range []*legacyNativeValueSnapshot{snapshot.key, snapshot.value} {
		bytes, err := child.retainedBytes()
		if err != nil {
			return 0, err
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes})
	}
	return heap.bytes, heap.err
}

func (snapshot *legacyNativeMapEntrySnapshot) close() {
	if snapshot != nil {
		snapshot.key.close()
		snapshot.value.close()
		*snapshot = legacyNativeMapEntrySnapshot{closed: true}
	}
}
