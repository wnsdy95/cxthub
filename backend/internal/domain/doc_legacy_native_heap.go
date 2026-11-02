package domain

import (
	"context"
	"fmt"
	"reflect"
)

const legacyNativeHeapMaxDepth = 1 << 16

func legacyNativeValueHeap(ctx context.Context, value reflect.Value) (docRangeHeaderHeapEstimate, error) {
	var heap docRangeHeaderHeapEstimate
	err := legacyNativeValueHeapWalk(ctx, value, 0, true, &heap)
	return heap, err
}

func legacyNativeValueHeapWalk(ctx context.Context, value reflect.Value, depth int, includeStorage bool, heap *docRangeHeaderHeapEstimate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if heap.err != nil {
		return heap.err
	}
	if !value.IsValid() {
		return nil
	}
	if depth > legacyNativeHeapMaxDepth {
		return fmt.Errorf("%w: legacy native heap reference depth", ErrValidation)
	}
	if includeStorage && value.Type().Size() != 0 {
		heap.allocation(1, int64(value.Type().Size()))
		if heap.err != nil {
			return heap.err
		}
	}
	switch value.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
	case reflect.String:
		heap.allocation(int64(value.Len()), 1)
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			if err := legacyNativeValueHeapWalk(ctx, value.Elem(), depth+1, true, heap); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for position := 0; position < value.NumField(); position++ {
			if err := legacyNativeValueHeapWalk(ctx, value.Field(position), depth+1, false, heap); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		length := value.Len()
		if value.Kind() == reflect.Slice {
			length = value.Cap()
			if width := value.Type().Elem().Size(); width != 0 {
				heap.allocation(int64(length), int64(width))
				if heap.err != nil {
					return heap.err
				}
			}
			value = value.Slice(0, length)
		}
		for position := 0; position < length; position++ {
			if err := legacyNativeValueHeapWalk(ctx, value.Index(position), depth+1, false, heap); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.IsNil() {
			break
		}
		width := docRangeHeaderHeapEstimate{bytes: 8}
		width.include(docRangeHeaderHeapEstimate{bytes: int64(value.Type().Key().Size())})
		width.include(docRangeHeaderHeapEstimate{bytes: int64(value.Type().Elem().Size())})
		if width.err != nil {
			heap.include(width)
			return heap.err
		}
		heap.allocation(1, 64)
		heap.allocation(int64(max(8, value.Len())), width.bytes)
		if heap.err != nil {
			return heap.err
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := legacyNativeValueHeapWalk(ctx, iterator.Key(), depth+1, false, heap); err != nil {
				return err
			}
			if err := legacyNativeValueHeapWalk(ctx, iterator.Value(), depth+1, false, heap); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unsupported legacy native heap kind %s", ErrValidation, value.Kind())
	}
	if heap.err != nil {
		return heap.err
	}
	return ctx.Err()
}
