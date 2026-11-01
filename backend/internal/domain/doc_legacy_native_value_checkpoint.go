package domain

import (
	"context"
	"encoding/binary"
	"math"
	"unsafe"
)

const legacyNativeValueCheckpointBytes = 40

const (
	legacyNativeCheckpointNull uint8 = iota
	legacyNativeCheckpointBool
	legacyNativeCheckpointFloat64
	legacyNativeCheckpointString
	legacyNativeCheckpointArray
	legacyNativeCheckpointObject
)

type legacyNativeValueCheckpointBounds struct {
	PayloadBytes uint64
	Nodes        uint64
}

type legacyNativeValueCheckpoint struct {
	Version   uint8
	Kind      uint8
	Nil       bool
	Bits      uint64
	Reference uint64
	Length    uint64
	Capacity  uint64
}

func legacyNativeValueCheckpointPayloadLimit() uint64 {
	return uint64(MaxFinalizedDocBytes)*6 + uint64(len(legacyNativeTypedNumberErrorPrefix)+len(legacyNativeTypedNumberErrorSuffix)+2)
}

func (state legacyNativeValueCheckpoint) validate(bounds legacyNativeValueCheckpointBounds) error {
	maximum := legacyNativeValueCheckpointPayloadLimit()
	if state.Version != 1 || bounds.PayloadBytes > maximum || bounds.Nodes > uint64(MaxFinalizedDocBytes) ||
		state.Length > uint64(int(^uint(0)>>1)) || state.Capacity > uint64(int(^uint(0)>>1)) {
		return ErrIntegrity
	}
	switch state.Kind {
	case legacyNativeCheckpointNull, legacyNativeCheckpointBool, legacyNativeCheckpointFloat64:
		if state.Nil || state.Reference != 0 || state.Length != 0 || state.Capacity != 0 {
			return ErrIntegrity
		}
		if state.Kind == legacyNativeCheckpointNull && state.Bits != 0 || state.Kind == legacyNativeCheckpointBool && state.Bits > 1 {
			return ErrIntegrity
		}
		if state.Kind == legacyNativeCheckpointFloat64 {
			value := math.Float64frombits(state.Bits)
			if math.IsInf(value, 0) || math.IsNaN(value) {
				return ErrIntegrity
			}
		}
	case legacyNativeCheckpointString:
		if state.Nil || state.Bits != 0 || state.Reference > bounds.PayloadBytes ||
			state.Length > bounds.PayloadBytes-state.Reference || state.Capacity < state.Length || state.Capacity > 2*maximum+64 {
			return ErrIntegrity
		}
	case legacyNativeCheckpointArray, legacyNativeCheckpointObject:
		if state.Bits != 0 || state.Length > bounds.Nodes {
			return ErrIntegrity
		}
		if state.Nil {
			if state.Reference != 0 || state.Length != 0 || state.Capacity != 0 {
				return ErrIntegrity
			}
		} else if state.Reference == 0 || state.Reference > bounds.Nodes {
			return ErrIntegrity
		}
		if state.Kind == legacyNativeCheckpointArray && (state.Capacity < state.Length || state.Capacity > uint64(MaxFinalizedDocBytes)) ||
			state.Kind == legacyNativeCheckpointObject && state.Capacity != 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeValueCheckpoint) encode(ctx context.Context, bounds legacyNativeValueCheckpointBounds) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := state.validate(bounds); err != nil {
		return nil, err
	}
	body := make([]byte, legacyNativeValueCheckpointBytes)
	body[0], body[1] = state.Version, state.Kind
	if state.Nil {
		body[2] = 1
	}
	binary.LittleEndian.PutUint64(body[8:16], state.Bits)
	binary.LittleEndian.PutUint64(body[16:24], state.Reference)
	binary.LittleEndian.PutUint64(body[24:32], state.Length)
	binary.LittleEndian.PutUint64(body[32:40], state.Capacity)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func decodeLegacyNativeValueCheckpoint(ctx context.Context, body []byte, bounds legacyNativeValueCheckpointBounds) (legacyNativeValueCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeValueCheckpoint{}, err
	}
	if len(body) != legacyNativeValueCheckpointBytes || body[2] > 1 {
		return legacyNativeValueCheckpoint{}, ErrIntegrity
	}
	for _, reserved := range body[3:8] {
		if reserved != 0 {
			return legacyNativeValueCheckpoint{}, ErrIntegrity
		}
	}
	state := legacyNativeValueCheckpoint{
		Version: body[0], Kind: body[1], Nil: body[2] == 1,
		Bits: binary.LittleEndian.Uint64(body[8:16]), Reference: binary.LittleEndian.Uint64(body[16:24]),
		Length: binary.LittleEndian.Uint64(body[24:32]), Capacity: binary.LittleEndian.Uint64(body[32:40]),
	}
	if err := state.validate(bounds); err != nil {
		return legacyNativeValueCheckpoint{}, err
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeValueCheckpoint{}, err
	}
	return state, nil
}

func (state legacyNativeValueCheckpoint) inline(ctx context.Context, bounds legacyNativeValueCheckpointBounds) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := state.validate(bounds); err != nil {
		return nil, false, err
	}
	switch state.Kind {
	case legacyNativeCheckpointNull:
		return nil, true, nil
	case legacyNativeCheckpointBool:
		return state.Bits == 1, true, nil
	case legacyNativeCheckpointFloat64:
		return math.Float64frombits(state.Bits), true, nil
	case legacyNativeCheckpointArray:
		if state.Nil {
			return []any(nil), true, nil
		}
	case legacyNativeCheckpointObject:
		if state.Nil {
			return map[string]any(nil), true, nil
		}
	}
	return nil, false, nil
}

type legacyNativeValueSnapshot struct {
	state  legacyNativeValueCheckpoint
	text   string
	closed bool
}

func (value legacyNativeAnyValue) checkpoint(ctx context.Context, reference uint64, bounds legacyNativeValueCheckpointBounds) (*legacyNativeValueSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := value.owner.matches(value.value); err != nil {
		return nil, err
	}
	if value.heap.err != nil {
		return nil, value.heap.err
	}
	if value.heap.bytes < 0 || value.excess < 0 || value.excess > value.heap.bytes || value.capacity < 0 {
		return nil, ErrIntegrity
	}
	state := legacyNativeValueCheckpoint{Version: 1, Reference: reference}
	var text string
	switch typed := value.value.(type) {
	case nil:
		state.Kind = legacyNativeCheckpointNull
	case bool:
		state.Kind = legacyNativeCheckpointBool
		if typed {
			state.Bits = 1
		}
	case float64:
		state.Kind, state.Bits = legacyNativeCheckpointFloat64, math.Float64bits(typed)
	case string:
		state.Kind, state.Length, state.Capacity = legacyNativeCheckpointString, uint64(len(typed)), uint64(value.capacity)
		text = typed
	case []any:
		state.Kind, state.Nil = legacyNativeCheckpointArray, typed == nil
		state.Length, state.Capacity = uint64(len(typed)), uint64(cap(typed))
	case map[string]any:
		state.Kind, state.Nil = legacyNativeCheckpointObject, typed == nil
		state.Length = uint64(len(typed))
	default:
		return nil, ErrIntegrity
	}
	if state.Kind != legacyNativeCheckpointString && value.capacity != 0 {
		return nil, ErrIntegrity
	}
	if err := state.validate(bounds); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &legacyNativeValueSnapshot{state: state, text: text}, nil
}

func (snapshot *legacyNativeValueSnapshot) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.closed || snapshot.state.Kind != legacyNativeCheckpointString ||
		snapshot.state.Length != uint64(len(snapshot.text)) || offset < 0 || offset > len(snapshot.text) ||
		length < 0 || length > LegacyCanonicalEncodingBytes || length > len(snapshot.text)-offset {
		return nil, ErrIntegrity
	}
	body := []byte(snapshot.text[offset : offset+length])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func (snapshot *legacyNativeValueSnapshot) retainedBytes() (int64, error) {
	if snapshot == nil || snapshot.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*snapshot)))
	if snapshot.state.Kind == legacyNativeCheckpointString {
		if snapshot.state.Capacity > uint64(int(^uint(0)>>1)) || snapshot.state.Capacity < uint64(len(snapshot.text)) {
			return 0, ErrIntegrity
		}
		heap.allocation(int64(snapshot.state.Capacity), 1)
	}
	return heap.bytes, heap.err
}

func (snapshot *legacyNativeValueSnapshot) close() {
	if snapshot != nil {
		*snapshot = legacyNativeValueSnapshot{closed: true}
	}
}
