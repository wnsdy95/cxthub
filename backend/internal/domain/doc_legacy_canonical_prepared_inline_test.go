package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyCanonicalPreparedInlineArrayRoundTrip(test *testing.T) {
	for _, values := range [][]any{{}, {nil}, {true, false, "", nil}, {"value-00001", "\ud55c🙂\"\\\n<>&", true, nil}, {strings.Repeat("\x00", 64)}, {nil, nil, nil, nil, nil, nil, nil, nil}} {
		storage := legacyCanonicalPreparedValueTestStorage(test)
		binding := HashContent([]byte("inline-array"))
		ref := legacyCanonicalPreparedValueTestWrite(test, values, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
		if storage.writes != 1 || len(storage.objects) != 1 {
			test.Fatal("small array did not use one immutable object", storage.writes, len(storage.objects))
		}
		value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
		array := value.(*legacyCanonicalPreparedArray)
		if array.values.length() != len(values) {
			test.Fatal("array length changed")
		}
		for index, expected := range values {
			if !reflect.DeepEqual(*array.values.at(index), expected) {
				test.Fatal("array item changed", index)
			}
		}
		if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
			test.Fatal("inline payload ownership", heap, expected)
		}
		if recaptured := legacyCanonicalPreparedValueTestWrite(test, value, nil, binding, storage, legacyCanonicalPreparedValueTestBudget); recaptured != ref {
			test.Fatal("recapture changed the logical value")
		}
	}
}

func TestLegacyCanonicalPreparedInlineArrayFallback(test *testing.T) {
	for _, values := range [][]any{nil, {1}, {1.0}, {json.Number("1")}, {[]any{}}, {map[string]any{}}, {"\xff"}, {strings.Repeat("x", 65)}, make([]any, 9)} {
		if body := legacyCanonicalPreparedPackArray(values); body != nil {
			test.Fatal("unsupported value bypassed original checkpoint path", values)
		}
	}
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("inline-array-slice"))
	slice := legacyCanonicalPreparedArraySlice{value: []any{"discarded", "keep", true, nil}, position: 1}
	ref := legacyCanonicalPreparedValueTestWrite(test, slice, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, _ := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
	array := value.(*legacyCanonicalPreparedArray)
	if array.values.length() != 4 || *array.values.at(0) != nil || *array.values.at(1) != "keep" || *array.values.at(2) != true || *array.values.at(3) != nil {
		test.Fatal("consumed array prefix was restored")
	}
}

func TestLegacyCanonicalPreparedInlineArrayRejectsInvalidRecords(test *testing.T) {
	for _, body := range []string{"null", "{}", "[1]", "[[]]", "[{}]", `["\ud800"]`, `["a",]`, "[true] false", "[ true ]", strings.Repeat(" ", legacyCanonicalPreparedInlineBytes+1), "[" + strings.Repeat("null,", 8) + "null]"} {
		if _, err := legacyCanonicalPreparedUnpackArray([]byte(body)); err == nil {
			test.Fatal("invalid inline array accepted", body)
		}
	}
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("inline-array-damage"))
	ref := legacyCanonicalPreparedValueTestWrite(test, []any{"original", true}, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	var record legacyCanonicalPreparedValueRecord
	if err := json.Unmarshal(storage.objects[ref.Hash], &record); err != nil {
		test.Fatal(err)
	}
	for _, damage := range []string{"wrong binding", "wrong tag", "count", "children", "text"} {
		changed := record
		switch damage {
		case "wrong binding":
			changed.Binding = HashContent([]byte("another"))
		case "wrong tag":
			changed.Tag = legacyCanonicalPreparedNil
		case "count":
			changed.Count = 1
		case "children":
			changed.Children = legacyNativeCheckpointTree{Ref: ref, Count: 1}
		case "text":
			changed.Texts[0].Bytes = 1
		}
		if err := changed.validate(binding); err == nil {
			test.Fatal("conflicting inline record accepted", damage)
		}
	}
	damaged := bytes.Clone(storage.objects[ref.Hash])
	damaged[0] ^= 1
	storage.objects[ref.Hash] = damaged
	work, err := newLegacyCanonicalPreparedValueRestore(context.Background(), ref, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	if _, err := work.step(context.Background()); err == nil {
		test.Fatal("inline array bypassed stored hash validation")
	}
}
