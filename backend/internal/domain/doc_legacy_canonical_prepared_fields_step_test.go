package domain

import (
	"context"
	"reflect"
	"testing"
)

func TestLegacyCanonicalPreparedFieldsKeepFormatWithoutEmptySetupSteps(test *testing.T) {
	ctx := context.Background()
	binding := HashContent([]byte("prepared-field-step"))
	expectedStorage := legacyCanonicalPreparedValueTestStorage(test)
	key := legacyCanonicalPreparedValueTestWrite(test, "name", nil, binding, expectedStorage, legacyCanonicalPreparedValueTestBudget)
	value := legacyCanonicalPreparedValueTestWrite(test, "value", nil, binding, expectedStorage, legacyCanonicalPreparedValueTestBudget)
	list := legacyNativeCheckpointListWriter[legacyCanonicalPreparedFieldState]{}
	defer list.close()
	if err := list.append(ctx, expectedStorage.io, legacyCanonicalPreparedFieldState{Key: key, Value: value}); err != nil {
		test.Fatal(err)
	}
	expected, err := list.finish(ctx, expectedStorage.io)
	if err != nil {
		test.Fatal(err)
	}
	fields := legacyCanonicalPages[legacyCanonicalKeyField]{}
	if err := fields.append(legacyCanonicalKeyField{key: "name", value: "value"}); err != nil {
		test.Fatal(err)
	}
	storage := legacyCanonicalPreparedValueTestStorage(test)
	work := &legacyCanonicalPreparedFieldsWrite{source: &fields, binding: binding, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget, keySkip: -1}
	defer work.close()
	for step := 0; step < 6; step++ {
		storage.bytes, storage.writes = 0, 0
		ready, err := work.step(ctx)
		if err != nil || ready != (step == 5) || storage.bytes > LegacyCanonicalEncodingBytes || storage.writes > 1 {
			test.Fatal("field setup changed the single child operation or write bound", step, ready, err, storage.bytes, storage.writes)
		}
	}
	if work.tree != expected || !reflect.DeepEqual(storage.objects, expectedStorage.objects) {
		test.Fatal("field setup optimization changed the original stored tree or bytes", work.tree, expected)
	}
}
