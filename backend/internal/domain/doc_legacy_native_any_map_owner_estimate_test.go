package domain

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func legacyNativeAnyMapPutEstimateOracle(test *testing.T, native *legacyNativeAnyMap, key string, capacity int) {
	test.Helper()
	got, err := native.putOwned(key, capacity, nil, docRangeHeaderHeapEstimate{}, 0)
	if err != nil {
		test.Fatal(err)
	}
	want := native.heap()
	native.retained(&want)
	if got != want || got.err != nil || !native.valid() {
		test.Fatal("committed estimate differs from independent current ownership", got, want)
	}
}

func TestLegacyNativeAnyMapPutEstimatePhysicalParity(test *testing.T) {
	for _, count := range []int{0, 1, 8, 9, 64, 65, 4096, 4097} {
		test.Run(fmt.Sprintf("count_%d", count), func(test *testing.T) {
			native, entries := legacyNativeAnyMapIDsOracleSeed(test, count)
			cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
			legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, min(1024, max(1, count)), count <= 1024)
			legacyNativeAnyMapPutEstimateOracle(test, native, "replacement", 64)
			legacyNativeAnyMapPutEstimateOracle(test, native, legacyNativeAnyMapIDsOracleKey(max(0, count-1)), 64)
		})
	}
}

func TestLegacyNativeAnyMapPutEstimateRejectsWithoutReceiptOrMutation(test *testing.T) {
	sentinel := errors.New("payload sentinel")
	for _, fixture := range []struct {
		name    string
		corrupt func(*legacyNativeAnyMap)
		value   docRangeHeaderHeapEstimate
		want    error
	}{
		{name: "nil_decoded", corrupt: func(native *legacyNativeAnyMap) { native.decoded = nil }, want: ErrIntegrity},
		{name: "nil_ownership", corrupt: func(native *legacyNativeAnyMap) { native.ownership = nil }, want: ErrIntegrity},
		{name: "invalid_first", corrupt: func(native *legacyNativeAnyMap) { native.firstID = 0 }, want: ErrIntegrity},
		{name: "last_cycle", corrupt: func(native *legacyNativeAnyMap) { native.link(native.lastID).nextID = native.lastID }, want: ErrIntegrity},
		{name: "dirty_head", corrupt: func(native *legacyNativeAnyMap) { native.dirtyHeadID = 0 }, want: ErrIntegrity},
		{name: "payload_error", corrupt: func(native *legacyNativeAnyMap) { native.payload.err = sentinel }, want: sentinel},
		{name: "negative_payload", corrupt: func(native *legacyNativeAnyMap) { native.payload.bytes = -1 }, want: ErrIntegrity},
		{name: "page_heap_overflow", corrupt: func(native *legacyNativeAnyMap) { native.links.nodes = int(^uint(0) >> 1) }, want: ErrIntegrity},
		{name: "value_error", value: docRangeHeaderHeapEstimate{err: sentinel}, want: sentinel},
		{name: "negative_value", value: docRangeHeaderHeapEstimate{bytes: -1}, want: ErrIntegrity},
		{name: "value_overflow", value: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}, want: ErrIntegrity},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			native, _ := legacyNativeAnyMapIDsOracleSeed(test, 9)
			if fixture.corrupt != nil {
				fixture.corrupt(native)
			}
			before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
			if native.decoded == nil {
				before.decoded = nil
			}
			if native.ownership == nil {
				before.ownership = nil
			}
			before.links.count, before.links.nodes, before.links.pages, before.links.depth = native.links.count, native.links.nodes, native.links.pages, native.links.depth
			got, err := native.putOwned("replacement", 64, nil, fixture.value, 0)
			if got != (docRangeHeaderHeapEstimate{}) || !errors.Is(err, fixture.want) || !reflect.DeepEqual(*native, before) {
				test.Fatal("rejected put returned an estimate or changed ownership", got, err, fixture.want)
			}
		})
	}
	var native *legacyNativeAnyMap
	got, err := native.putOwned("", 0, nil, docRangeHeaderHeapEstimate{}, 0)
	if got != (docRangeHeaderHeapEstimate{}) || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil owner returned an estimate", got, err)
	}
}
