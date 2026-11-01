package domain

import (
	"context"
	"math"
	"reflect"
)

func (value legacyNativeAnyValue) validateOwnership() error {
	if value.heap.err != nil {
		return value.heap.err
	}
	if value.heap.bytes < 0 || value.excess < 0 || value.excess > value.heap.bytes || value.capacity < 0 {
		return ErrIntegrity
	}
	if err := value.owner.matches(value.value); err != nil {
		return err
	}
	switch decoded := value.value.(type) {
	case nil, bool:
	case float64:
		if math.IsNaN(decoded) || math.IsInf(decoded, 0) {
			return ErrIntegrity
		}
	case string:
		if value.capacity < len(decoded) || value.excess%2 != 0 || value.excess/2 != int64(value.capacity-len(decoded)) {
			return ErrIntegrity
		}
		return nil
	case map[string]any:
		if decoded != nil && value.owner.object == nil {
			return ErrIntegrity
		}
		if value.owner.object != nil && (value.heap != value.owner.object.heap() || value.excess != value.owner.object.backingExcess()) {
			return ErrIntegrity
		}
	case []any:
		if decoded != nil && value.owner.array == nil {
			return ErrIntegrity
		}
		if value.owner.array != nil && (value.heap != value.owner.array.heap() || value.excess != value.owner.array.backingExcess()) {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	if value.capacity != 0 {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeAnyValueOwner struct {
	object   *legacyNativeAnyMap
	array    *legacyNativeAnyArray
	revision uint64
	bytes    int64
}

func (owner legacyNativeAnyValueOwner) validate() error {
	if owner.object == nil && owner.array == nil {
		if owner.revision != 0 || owner.bytes != 0 {
			return ErrIntegrity
		}
		return nil
	}
	if owner.object != nil && owner.array != nil || owner.bytes < 0 {
		return ErrIntegrity
	}
	var retained docRangeHeaderHeapEstimate
	if owner.object != nil {
		if owner.object.revision != owner.revision {
			return ErrConflict
		}
		owner.object.retained(&retained)
	} else {
		if owner.array.revision != owner.revision {
			return ErrConflict
		}
		owner.array.retained(&retained)
	}
	if retained.err != nil {
		return retained.err
	}
	if retained.bytes != owner.bytes {
		return ErrIntegrity
	}
	return nil
}

func (owner legacyNativeAnyValueOwner) matches(value any) error {
	if err := owner.validate(); err != nil {
		return err
	}
	if owner.object != nil {
		decoded, ok := value.(map[string]any)
		if !ok || reflect.ValueOf(decoded).Pointer() != reflect.ValueOf(owner.object.values()).Pointer() {
			return ErrIntegrity
		}
	}
	if owner.array != nil {
		decoded, ok := value.([]any)
		owned := owner.array.values()
		if !ok || len(decoded) != len(owned) || cap(decoded) != cap(owned) ||
			reflect.ValueOf(decoded).Pointer() != reflect.ValueOf(owned).Pointer() {
			return ErrIntegrity
		}
	}
	return nil
}

func (owner legacyNativeAnyValueOwner) mapCheckpoint(ctx context.Context, full bool) (*legacyNativeAnyMapCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.validate(); err != nil {
		return nil, err
	}
	if owner.object == nil {
		return nil, ErrIntegrity
	}
	return owner.object.checkpoint(ctx, full)
}

func (owner legacyNativeAnyValueOwner) arrayValue(ctx context.Context, position int) (legacyNativeAnyValue, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if err := owner.validate(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if owner.array == nil || !owner.array.tracked || position < 0 || position >= len(owner.array.decoded) ||
		!owner.array.children.valid() || owner.array.children.length() != len(owner.array.decoded) {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	child := owner.array.children.at(position)
	if child == nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	value := legacyNativeAnyValue{value: owner.array.values()[position], heap: docRangeHeaderHeapEstimate{bytes: child.heapBytes},
		excess: child.excess, capacity: child.capacity, owner: child.owner}
	if err := value.validateOwnership(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeAnyValue{}, err
	}
	return value, nil
}

type legacyNativeAnyArrayChild struct {
	heapBytes int64
	excess    int64
	capacity  int
	owner     legacyNativeAnyValueOwner
}
