package domain

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"unsafe"
)

func TestLegacyNativeAnyMapIDsReviewRejectsUnownedEndpoints(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		corrupt func(*legacyNativeAnyMap)
	}{
		{name: "last_owned_id", corrupt: func(native *legacyNativeAnyMap) {
			row := native.ownership["last"]
			row.linkID = 0
			native.ownership["last"] = row
		}},
		{name: "last_key", corrupt: func(native *legacyNativeAnyMap) {
			native.link(native.lastID).canonicalKey = "unowned"
		}},
		{name: "dirty_head_owned_id", corrupt: func(native *legacyNativeAnyMap) {
			row := native.ownership["first"]
			row.linkID = 0
			native.ownership["first"] = row
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			native := newLegacyNativeAnyMap()
			legacyNativeAnyMapCheckpointOraclePut(test, native, "first", 5, nil, 0)
			legacyNativeAnyMapCheckpointOraclePut(test, native, "last", 4, nil, 0)
			fixture.corrupt(native)
			before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
			storageBefore := native.links
			if err := native.put("next", 4, nil, docRangeHeaderHeapEstimate{}, 0); !errors.Is(err, ErrIntegrity) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
				test.Fatal("unowned endpoint was mutated before rejection", err)
			}
		})
	}
}

func TestLegacyNativeAnyMapIDsReviewRejectsCombinedHeapOverflow(test *testing.T) {
	native := newLegacyNativeAnyMap()
	_, futureLinks, err := native.links.appendRequirements()
	if err != nil {
		test.Fatal(err)
	}
	forecast := legacyNativeAnyMapBacking(1, int64(unsafe.Sizeof(any(nil))))
	forecast.include(legacyNativeAnyMapMetadata(1, futureLinks))
	if forecast.err != nil {
		test.Fatal(forecast.err)
	}
	before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
	storageBefore := native.links
	valueHeap := docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - forecast.bytes}
	if err := native.put("", 0, nil, valueHeap, 0); !errors.Is(err, ErrIntegrity) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
		test.Fatal("combined native map heap overflow allocated or mutated", err)
	}
}
