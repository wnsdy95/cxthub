package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestLegacyNativeAnyMapCheckpointAckRejectsRepeatedNodes(test *testing.T) {
	for _, limit := range []int{2, 3} {
		test.Run(fmt.Sprintf("limit_%d", limit), func(test *testing.T) {
			native := newLegacyNativeAnyMap()
			var expected []legacyNativeAnyMapCheckpointEntry
			for _, key := range []string{"alpha", "beta", "gamma", "delta"} {
				expected = append(expected, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), nil, 0))
			}
			cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
			row := native.ownership["beta"]
			native.link(row.linkID).dirtyNextID = native.ownership["alpha"].linkID
			native.ownership["beta"] = row
			before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
			complete, err := cursor.acknowledge(context.Background(), limit)
			if complete || !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(*native, before) {
				test.Fatalf("cyclic ack changed rows or counts: complete=%v error=%v dirty=%d want=%d", complete, err, native.dirtyCount, before.dirtyCount)
			}
		})
	}
}
