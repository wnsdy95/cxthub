package domain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeAnyMapTestValueHeap(test *testing.T, value any) docRangeHeaderHeapEstimate {
	test.Helper()
	estimate, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(value))
	if err != nil {
		test.Fatal(err)
	}
	return estimate
}

func legacyNativeAnyMapTestAccounting(test *testing.T, native *legacyNativeAnyMap, excess int64) {
	test.Helper()
	var minimum docRangeHeaderHeapEstimate
	if err := legacyNativeValueHeapWalk(context.Background(), reflect.ValueOf(native.values()), 0, true, &minimum); err != nil {
		test.Fatal(err)
	}
	minimum.include(docRangeHeaderHeapEstimate{bytes: excess})
	if actual := native.heap(); actual.err != nil || minimum.err != nil || actual.bytes != minimum.bytes || native.backingExcess() != excess {
		test.Fatal("map lost dynamic payload or actual backing excess", actual, minimum, native.backingExcess(), excess)
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*native)))
	expected.allocation(1, 64)
	expected.allocation(int64(max(8, len(native.values()))), int64(unsafe.Sizeof("")+unsafe.Sizeof(legacyNativeAnyMapOwnership{}))+8)
	native.links.retained(&expected)
	for _, row := range native.ownership {
		expected.include(legacyNativeAnyOwnerMetadataOracle(test, row.owner))
	}
	var retained docRangeHeaderHeapEstimate
	native.retained(&retained)
	if retained.err != nil || expected.err != nil || retained.bytes != expected.bytes {
		test.Fatal("scratch ledger storage double-counted keys or omitted its map backing", retained, expected)
	}
}

func TestLegacyNativeAnyMapFreshEmptyAndBucketAccounting(test *testing.T) {
	native, other := newLegacyNativeAnyMap(), newLegacyNativeAnyMap()
	if native == nil || native.values() == nil || len(native.values()) != 0 || other.values() == nil {
		test.Fatal("fresh ledger did not own a nonnil empty object")
	}
	legacyNativeAnyMapTestAccounting(test, native, 0)
	visible := native.values()
	for position := 0; position < 10; position++ {
		key := fmt.Sprintf("key-%d", position)
		value := float64(position) + 0.25
		if err := native.put(key, len(key), value, legacyNativeAnyMapTestValueHeap(test, value), 0); err != nil {
			test.Fatal(err)
		}
		if visible[key] != value || len(visible) != position+1 || len(other.values()) != 0 {
			test.Fatal("put replaced its outer object or shared a fresh object's map")
		}
		legacyNativeAnyMapTestAccounting(test, native, 0)
	}
}

func TestLegacyNativeAnyMapReplacementDropsPriorChildren(test *testing.T) {
	native := newLegacyNativeAnyMap()
	prior := map[string]any{"old": strings.Repeat("payload", 32), "nested": []any{true, "prior"}}
	values := []any{
		prior,
		map[string]any{"new": float64(1.25)},
		[]any{"array", map[string]any{"child": false}},
		"scalar",
		nil,
		map[string]any{},
		map[string]any(nil),
		[]any{},
		[]any(nil),
		false,
	}
	if err := native.put("keep", 4, "untouched", legacyNativeAnyMapTestValueHeap(test, "untouched"), 0); err != nil {
		test.Fatal(err)
	}
	for position, value := range values {
		if err := native.put("replace", 7, value, legacyNativeAnyMapTestValueHeap(test, value), 0); err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(native.values()["replace"], value) || len(native.values()) != 2 || native.values()["keep"] != "untouched" {
			test.Fatal("generic child replacement merged or retained a prior payload", position, native.values())
		}
		legacyNativeAnyMapTestAccounting(test, native, 0)
	}
	prior["later"] = "not retained"
	if native.values()["replace"] != false {
		test.Fatal("an old nested object remained the active child")
	}
}

func TestLegacyNativeAnyMapKeyAndValueCapacityReplacement(test *testing.T) {
	native := newLegacyNativeAnyMap()
	key := strings.Clone("key")
	var builder strings.Builder
	builder.Grow(64)
	builder.WriteString("value")
	value := builder.String()
	valueHeap := legacyNativeAnyMapTestValueHeap(test, value)
	valueExcess := 2 * int64(builder.Cap()-len(value))
	valueHeap.include(docRangeHeaderHeapEstimate{bytes: valueExcess})
	if err := native.put(key, 64, value, valueHeap, valueExcess); err != nil {
		test.Fatal(err)
	}
	for storedKey := range native.values() {
		if unsafe.StringData(storedKey) != unsafe.StringData(key) {
			test.Fatal("destination map copied key text")
		}
	}
	for storedKey := range native.ownership {
		if unsafe.StringData(storedKey) != unsafe.StringData(key) {
			test.Fatal("scratch ledger copied key text")
		}
	}
	legacyNativeAnyMapTestAccounting(test, native, 2*int64(64-len(key))+valueExcess)
	for _, fixture := range []struct {
		capacity int
		retained int
	}{
		{capacity: len(key), retained: 64},
		{capacity: 128, retained: 128},
		{capacity: len(key), retained: 128},
	} {
		if err := native.put(strings.Clone(key), fixture.capacity, nil, docRangeHeaderHeapEstimate{}, 0); err != nil {
			test.Fatal(err)
		}
		legacyNativeAnyMapTestAccounting(test, native, 2*int64(fixture.retained-len(key)))
	}
	if native.ownership[key].keyCapacity != 128 {
		test.Fatal("equal key replacement forgot an earlier backing capacity")
	}
	if err := native.put(key, len(key), value, valueHeap, valueExcess); err != nil {
		test.Fatal(err)
	}
	legacyNativeAnyMapTestAccounting(test, native, 2*int64(128-len(key))+valueExcess)
	if err := native.put("second", 16, value, valueHeap, valueExcess); err != nil {
		test.Fatal(err)
	}
	legacyNativeAnyMapTestAccounting(test, native, 2*int64(128-len(key))+2*int64(16-len("second"))+2*valueExcess)
	if err := native.put("second", len("second"), nil, docRangeHeaderHeapEstimate{}, 0); err != nil {
		test.Fatal(err)
	}
	legacyNativeAnyMapTestAccounting(test, native, 2*int64(128-len(key))+2*int64(16-len("second"))+valueExcess)
}

func TestLegacyNativeAnyMapEmptyKeyCapacity(test *testing.T) {
	native := newLegacyNativeAnyMap()
	for _, fixture := range []struct {
		capacity int
		retained int
	}{
		{capacity: 0, retained: 0},
		{capacity: 12, retained: 12},
		{capacity: 0, retained: 12},
		{capacity: 24, retained: 24},
	} {
		if err := native.put("", fixture.capacity, nil, docRangeHeaderHeapEstimate{}, 0); err != nil {
			test.Fatal(err)
		}
		var keyHeap docRangeHeaderHeapEstimate
		keyHeap.allocation(int64(fixture.retained), 1)
		legacyNativeAnyMapTestAccounting(test, native, keyHeap.bytes)
	}
}

func TestLegacyNativeAnyMapRetainedErrorAccounting(test *testing.T) {
	native := newLegacyNativeAnyMap()
	if err := native.put("key", 3, "value", legacyNativeAnyMapTestValueHeap(test, "value"), 0); err != nil {
		test.Fatal(err)
	}
	failure := errors.New("retained estimate failed")
	estimate := docRangeHeaderHeapEstimate{bytes: 17, err: failure}
	native.retained(&estimate)
	if estimate.bytes != 17 || !errors.Is(estimate.err, failure) {
		test.Fatal("scratch accounting changed an earlier failure", estimate)
	}
	estimate = docRangeHeaderHeapEstimate{bytes: math.MaxInt64}
	native.retained(&estimate)
	if !errors.Is(estimate.err, ErrIntegrity) {
		test.Fatal("scratch accounting silently overflowed", estimate)
	}
}

func TestLegacyNativeAnyMapInvalidPutIsAtomic(test *testing.T) {
	failure := errors.New("child heap failed")
	fixtures := []struct {
		name     string
		key      string
		capacity int
		heap     docRangeHeaderHeapEstimate
		excess   int64
		want     error
	}{
		{name: "negative_capacity", key: "kept", capacity: -1, want: ErrIntegrity},
		{name: "short_capacity", key: "kept", capacity: 3, want: ErrIntegrity},
		{name: "negative_heap", key: "kept", capacity: 4, heap: docRangeHeaderHeapEstimate{bytes: -1}, want: ErrIntegrity},
		{name: "heap_error", key: "kept", capacity: 4, heap: docRangeHeaderHeapEstimate{err: failure}, want: failure},
		{name: "negative_excess", key: "kept", capacity: 4, excess: -1, want: ErrIntegrity},
		{name: "excess_above_heap", key: "kept", capacity: 4, heap: docRangeHeaderHeapEstimate{bytes: 1}, excess: 2, want: ErrIntegrity},
		{name: "key_allocation_overflow", key: "kept", capacity: int(^uint(0) >> 1), want: ErrIntegrity},
		{name: "child_sum_overflow", key: "new", capacity: 3, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}, want: ErrIntegrity},
		{name: "map_backing_overflow", key: "kept", capacity: 4, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - 100}, want: ErrIntegrity},
		{name: "scratch_backing_overflow", key: "kept", capacity: 4, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - 1500}, want: ErrIntegrity},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			native := newLegacyNativeAnyMap()
			if err := native.put("kept", 16, "original", legacyNativeAnyMapTestValueHeap(test, "original"), 0); err != nil {
				test.Fatal(err)
			}
			beforeHeap, beforeExcess, beforeOwnership := native.heap(), native.backingExcess(), native.ownership["kept"]
			var beforeRetained docRangeHeaderHeapEstimate
			native.retained(&beforeRetained)
			err := native.put(fixture.key, fixture.capacity, nil, fixture.heap, fixture.excess)
			if !errors.Is(err, fixture.want) {
				test.Fatal("invalid ownership was accepted or changed its error", err, fixture.want)
			}
			var afterRetained docRangeHeaderHeapEstimate
			native.retained(&afterRetained)
			if native.heap() != beforeHeap || native.backingExcess() != beforeExcess || native.ownership["kept"] != beforeOwnership ||
				afterRetained != beforeRetained || len(native.values()) != 1 || native.values()["kept"] != "original" {
				test.Fatal("rejected put changed committed values or accounting")
			}
			if err := native.put("kept", 4, nil, docRangeHeaderHeapEstimate{}, 0); err != nil {
				test.Fatal("rejected put poisoned later valid replacement", err)
			}
			legacyNativeAnyMapTestAccounting(test, native, 2*int64(16-len("kept")))
		})
	}
}

func TestLegacyNativeAnyMapNilAndInvalidLedger(test *testing.T) {
	var missing *legacyNativeAnyMap
	if missing.values() != nil || missing.backingExcess() != 0 || !errors.Is(missing.heap().err, ErrIntegrity) ||
		!errors.Is(missing.put("", 0, nil, docRangeHeaderHeapEstimate{}, 0), ErrIntegrity) {
		test.Fatal("nil ledger did not fail closed")
	}
	var retained docRangeHeaderHeapEstimate
	missing.retained(&retained)
	if retained != (docRangeHeaderHeapEstimate{}) {
		test.Fatal("nil ledger retained scratch storage")
	}
	zero := &legacyNativeAnyMap{}
	if !errors.Is(zero.put("", 0, nil, docRangeHeaderHeapEstimate{}, 0), ErrIntegrity) || !errors.Is(zero.heap().err, ErrIntegrity) {
		test.Fatal("uninitialized ledger accepted a put")
	}
	zero.retained(&retained)
	if !errors.Is(retained.err, ErrIntegrity) {
		test.Fatal("uninitialized ledger exposed valid scratch accounting")
	}
	native := newLegacyNativeAnyMap()
	if err := native.put("kept", 4, nil, docRangeHeaderHeapEstimate{}, 0); err != nil {
		test.Fatal(err)
	}
	delete(native.values(), "kept")
	if !errors.Is(native.put("new", 3, nil, docRangeHeaderHeapEstimate{}, 0), ErrIntegrity) || !errors.Is(native.heap().err, ErrIntegrity) {
		test.Fatal("mismatched destination and ownership maps did not fail closed")
	}
}
