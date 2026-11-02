package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestLegacyCanonicalPreparedFieldsArraySameWire(test *testing.T) {
	var fields legacyCanonicalPages[legacyCanonicalKeyField]
	for position := 0; position < 257; position++ {
		if err := fields.append(legacyCanonicalKeyField{key: fmt.Sprintf("field-%05d", position), value: []any{fmt.Sprintf("value-%05d", position), "\ud55c🙂\"\\\n<>&", true, nil}, childKeys: position}); err != nil {
			test.Fatal(err)
		}
	}
	for _, cut := range []struct{ keyAt, valueAt, keySkip int }{{0, 0, -1}, {10, 15, 20}} {
		var trees [2]legacyNativeCheckpointTree
		var stores [2]*legacyCanonicalPreparedValueTestStore
		var calls [2][]legacyNativeCheckpointRef
		var steps [2]int
		for index := range stores {
			storage := legacyCanonicalPreparedValueTestStorage(test)
			stores[index] = storage
			put := storage.io.Put
			storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				calls[index] = append(calls[index], legacyNativeCheckpointRef{Hash: hash, Bytes: len(body)})
				return put(ctx, hash, body)
			}
			work := &legacyCanonicalPreparedFieldsWrite{source: &fields, binding: HashContent([]byte("array-fields")), store: storage.io, budget: legacyCanonicalPreparedValueTestBudget,
				keyAt: cut.keyAt, valueAt: cut.valueAt, keySkip: cut.keySkip}
			defer work.close()
			step := work.step
			if index == 0 {
				step = work.stepOnce
			}
			for attempts := 0; attempts < 10000; attempts++ {
				storage.bytes, storage.writes = 0, 0
				before := work.position
				steps[index]++
				ready, err := step(context.Background())
				if err != nil || storage.bytes > LegacyCanonicalEncodingBytes || storage.writes > 8 || work.position-before > 1 {
					test.Fatal("array field exceeded unchanged quantum", storage.bytes, storage.writes, err)
				}
				if retained, err := work.retainedBytes(); err != nil || retained > work.budget {
					test.Fatal("array field exceeded heap budget", retained, err)
				}
				if ready {
					trees[index] = work.tree
					break
				}
			}
		}
		if !trees[0].valid() || trees[0] != trees[1] || !reflect.DeepEqual(stores[0].objects, stores[1].objects) || !reflect.DeepEqual(calls[0], calls[1]) || steps[0] <= steps[1] {
			test.Fatal("array batching changed wire, ordered CAS or failed to reduce steps", steps)
		}
		test.Logf("original_steps=%d batched_steps=%d puts=%d", steps[0], steps[1], len(calls[1]))
	}
}

func TestLegacyCanonicalPreparedFieldsArrayFailures(test *testing.T) {
	for _, mode := range []string{"write", "cancel", "budget"} {
		for failAt := 1; failAt <= 3; failAt++ {
			test.Run(fmt.Sprintf("%s/%d", mode, failAt), func(test *testing.T) {
				var fields legacyCanonicalPages[legacyCanonicalKeyField]
				if err := fields.append(legacyCanonicalKeyField{key: "key", value: []any{"short", true}}); err != nil {
					test.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("write failed")
				work := &legacyCanonicalPreparedFieldsWrite{source: &fields, binding: HashContent([]byte("array-failure")), budget: legacyCanonicalPreparedValueTestBudget, keySkip: -1}
				defer work.close()
				calls := 0
				work.store.Put = func(context.Context, ContentHash, []byte) error {
					calls++
					if calls == failAt {
						switch mode {
						case "write":
							return failure
						case "cancel":
							cancel()
						case "budget":
							work.budget = 1
						}
					}
					return nil
				}
				ready, err := work.step(ctx)
				want := map[string]error{"write": failure, "cancel": context.Canceled, "budget": ErrDocumentResourceLimit}[mode]
				if ready || !errors.Is(err, want) || calls != failAt || work.position != 0 {
					test.Fatal("batch crossed failed/canceled/over-budget IO", ready, err, calls, work.position)
				}
			})
		}
	}
}
