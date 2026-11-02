package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyNativeAnyMapEndpointLayoutsPreserveOwnershipAndOrder(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		updates []string
	}{
		{name: "head_tail_last", updates: []string{"last"}},
		{name: "head_last", updates: []string{"last", "first"}},
		{name: "tail_last", updates: []string{"first", "last"}},
		{name: "neither_last", updates: []string{"first", "middle"}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			cleanKey := ""
			for _, candidate := range []string{"first", "middle", "last"} {
				updated := false
				for _, key := range fixture.updates {
					updated = updated || candidate == key
				}
				if !updated {
					cleanKey = candidate
					break
				}
			}
			for _, targetKey := range []string{"next", cleanKey} {
				test.Run("target_"+targetKey, func(test *testing.T) {
					for _, corruptKey := range []string{"", fixture.updates[0], fixture.updates[len(fixture.updates)-1], "last"} {
						test.Run("corrupt_"+corruptKey, func(test *testing.T) {
							native := newLegacyNativeAnyMap()
							entries := make([]legacyNativeAnyMapCheckpointEntry, 0, 3)
							for _, key := range []string{"first", "middle", "last"} {
								entries = append(entries, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), nil, 0))
							}
							cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
							legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 3, true)
							delta := make([]legacyNativeAnyMapCheckpointEntry, 0, len(fixture.updates)+1)
							for _, key := range fixture.updates {
								delta = append(delta, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), true, 0))
							}
							if corruptKey != "" {
								row := native.ownership[corruptKey]
								row.linkID = 0
								native.ownership[corruptKey] = row
								before, storageBefore := legacyNativeAnyMapCheckpointOracleSnapshot(native), native.links
								if err := native.put(targetKey, len(targetKey), nil, docRangeHeaderHeapEstimate{}, 0); !errors.Is(err, ErrIntegrity) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
									test.Fatal("coalesced endpoint check missed a distinct owner or mutated before rejection", corruptKey, err)
								}
								return
							}
							delta = append(delta, legacyNativeAnyMapCheckpointOraclePut(test, native, targetKey, len(targetKey), false, 0))
							cursor = legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta)
							legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, false)
							legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta[1:])
							legacyNativeAnyMapCheckpointOraclePut(test, native, targetKey, len(targetKey), true, 0)
							if _, _, err := cursor.next(context.Background()); !errors.Is(err, ErrConflict) {
								test.Fatal("cursor remained usable after an actual source revision change", err)
							}
						})
					}
				})
			}
		})
	}
}
