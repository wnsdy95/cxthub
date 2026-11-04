package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyCanonicalInlineFieldsWrite(test *testing.T, fields *legacyCanonicalPages[legacyCanonicalKeyField], binding ContentHash, storage *legacyCanonicalPreparedValueTestStore, keyAt, valueAt, keySkip int) (legacyNativeCheckpointTree, int) {
	test.Helper()
	work := &legacyCanonicalPreparedFieldsWrite{source: fields, binding: binding, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget, keyAt: keyAt, valueAt: valueAt, keySkip: keySkip}
	defer work.close()
	writes := 0
	for steps := 0; steps < 100000; steps++ {
		storage.bytes, storage.writes = 0, 0
		position := work.position
		ready, err := work.step(context.Background())
		if err != nil || storage.bytes > LegacyCanonicalEncodingBytes || storage.writes > 8 || work.position-position > 1 {
			test.Fatal("field write quantum", steps, storage.bytes, storage.writes, err)
		}
		writes += storage.writes
		if retained, err := work.retainedBytes(); err != nil || retained > work.budget {
			test.Fatal("field write heap", retained, err)
		}
		if ready {
			return work.tree, writes
		}
	}
	test.Fatal("field write did not complete")
	return legacyNativeCheckpointTree{}, 0
}

func legacyCanonicalInlineFieldsRestore(test *testing.T, tree legacyNativeCheckpointTree, binding ContentHash, storage *legacyCanonicalPreparedValueTestStore) ([]legacyCanonicalKeyField, docRangeHeaderHeapEstimate) {
	test.Helper()
	reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](tree, storage.io)
	if err != nil {
		test.Fatal(err)
	}
	work := &legacyCanonicalPreparedFieldsRestore{reader: reader, store: storage.io, binding: binding, budget: legacyCanonicalPreparedValueTestBudget}
	defer work.close()
	for steps := 0; steps < 100000; steps++ {
		storage.bytes, storage.reads = 0, 0
		ready, err := work.step(context.Background())
		if err != nil || storage.bytes > LegacyCanonicalEncodingBytes || storage.reads > 8 {
			test.Fatal("field restore quantum", steps, storage.bytes, storage.reads, err)
		}
		if retained, err := work.retainedBytes(); err != nil || retained > work.budget {
			test.Fatal("field restore heap", retained, err)
		}
		if ready {
			fields := make([]legacyCanonicalKeyField, work.fields.length())
			for position := range fields {
				fields[position] = *work.fields.at(position)
			}
			var expected docRangeHeaderHeapEstimate
			for _, field := range fields {
				expected.include(legacyCanonicalPreparedValueTestHeap(test, field.key))
				expected.include(legacyCanonicalPreparedValueTestHeap(test, field.value))
			}
			if work.payload != expected {
				test.Fatal("restored field input heap differs from independent scalar accounting", work.payload, expected)
			}
			return fields, work.payload
		}
	}
	test.Fatal("field restore did not complete")
	return nil, docRangeHeaderHeapEstimate{}
}

func legacyCanonicalInlineFieldsOldWrite(test *testing.T, fields []legacyCanonicalKeyField, binding ContentHash, storage *legacyCanonicalPreparedValueTestStore) legacyNativeCheckpointTree {
	test.Helper()
	var list legacyNativeCheckpointListWriter[legacyCanonicalPreparedFieldState]
	defer list.close()
	for _, field := range fields {
		key := legacyCanonicalPreparedValueTestWrite(test, field.key, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
		value := legacyCanonicalPreparedValueTestWrite(test, field.value, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
		if err := list.append(context.Background(), storage.io, legacyCanonicalPreparedFieldState{Key: key, Value: value, ChildKeys: field.childKeys}); err != nil {
			test.Fatal(err)
		}
	}
	tree, err := list.finish(context.Background(), storage.io)
	if err != nil {
		test.Fatal(err)
	}
	return tree
}

func TestLegacyCanonicalPreparedFieldsInlinePrefixAndLegacyHeap(test *testing.T) {
	var fields legacyCanonicalPages[legacyCanonicalKeyField]
	values := []any{"short", "\ud55c\U0001f642", true, false, nil, "", strings.Repeat("long", 32), float64(42)}
	for position := 0; position < 40; position++ {
		if err := fields.append(legacyCanonicalKeyField{key: fmt.Sprintf("k%02d", position), value: values[position%len(values)], childKeys: position}); err != nil {
			test.Fatal(err)
		}
	}
	binding := HashContent([]byte("inline-field-prefix"))
	for _, prefix := range [][3]int{{0, 0, -1}, {17, 17, 17}, {40, 19, -1}, {40, 40, -1}} {
		test.Run(fmt.Sprint(prefix), func(test *testing.T) {
			expected := make([]legacyCanonicalKeyField, fields.length())
			for position := range expected {
				expected[position] = *fields.at(position)
				if position < prefix[0] || position == prefix[2] {
					expected[position].key = ""
				}
				if position < prefix[1] {
					expected[position].value = nil
				}
			}
			storage := legacyCanonicalPreparedValueTestStorage(test)
			tree, _ := legacyCanonicalInlineFieldsWrite(test, &fields, binding, storage, prefix[0], prefix[1], prefix[2])
			got, heap := legacyCanonicalInlineFieldsRestore(test, tree, binding, storage)
			oldStorage := legacyCanonicalPreparedValueTestStorage(test)
			oldTree := legacyCanonicalInlineFieldsOldWrite(test, expected, binding, oldStorage)
			old, oldHeap := legacyCanonicalInlineFieldsRestore(test, oldTree, binding, oldStorage)
			if !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(got, old) || heap != oldHeap {
				test.Fatal("inline changed legacy restore semantics, consumed prefix, or heap", heap, oldHeap)
			}
			for position := 0; position < fields.length(); position++ {
				if fields.at(position).key != fmt.Sprintf("k%02d", position) || !reflect.DeepEqual(fields.at(position).value, values[position%len(values)]) || fields.at(position).childKeys != position {
					test.Fatal("checkpoint mutated source", position)
				}
			}
		})
	}
}

func TestLegacyCanonicalPreparedFieldsInlineLimits(test *testing.T) {
	binding := HashContent([]byte("inline-field-limits"))
	fixtures := []struct {
		key   string
		value any
		want  bool
	}{
		{"k", strings.Repeat("x", 63), true},
		{"k", strings.Repeat("x", 64), false},
		{"k", strings.Repeat("\ud55c", 21), true},
		{"kk", strings.Repeat("\ud55c", 21), false},
		{"k", "\xff", false},
		{"\xff", "x", false},
		{"k", float64(1), false},
		{"k", []any{"x"}, false},
		{"k", map[string]any{"x": true}, false},
		{"k", nil, true},
		{"k", true, true},
	}
	for _, fixture := range fixtures {
		for _, consumed := range []int{0, 1} {
			work := &legacyCanonicalPreparedFieldsWrite{binding: binding, keyAt: consumed, valueAt: consumed, keySkip: -1}
			if err := work.inlineField(&legacyCanonicalKeyField{key: fixture.key, value: fixture.value}); err != nil || (work.record.Inline != nil) != fixture.want {
				test.Fatal("inline eligibility changed original field bounds", fixture, consumed, err)
			}
		}
	}
	for _, size := range []int{511, 512, 513} {
		found := false
		for controls := 0; controls <= 63 && !found; controls++ {
			for plain := 0; plain <= 63-controls; plain++ {
				value := strings.Repeat("\x00", controls) + strings.Repeat("x", plain)
				record := legacyCanonicalPreparedFieldState{Inline: &legacyCanonicalPreparedFieldInline{Binding: binding, Pair: legacyCanonicalPreparedPackArray([]any{"k", value})}}
				body, err := json.Marshal(record)
				if err != nil {
					test.Fatal(err)
				}
				if len(body) != size {
					continue
				}
				found = true
				work := &legacyCanonicalPreparedFieldsWrite{binding: binding, keySkip: -1}
				if err := work.inlineField(&legacyCanonicalKeyField{key: "k", value: value}); err != nil || (work.record.Inline != nil) != (size <= 512) {
					test.Fatal("encoded field bound", size, err)
				}
				_, _, err = record.inlineValues(binding)
				if (err == nil) != (size <= 512) {
					test.Fatal("restore encoded field bound", size, err)
				}
				break
			}
		}
		if !found {
			test.Fatal("missing exact encoded boundary", size)
		}
	}
}

func TestLegacyCanonicalPreparedFieldsInlineMalformed(test *testing.T) {
	binding := HashContent([]byte("inline-field-malformed"))
	for _, malformed := range []string{"binding", "invalid-binding", "key-ref", "value-ref", "child-keys", "number", "nested", "key-type", "arity", "text-limit", "noncanonical", "unknown"} {
		test.Run(malformed, func(test *testing.T) {
			record := legacyCanonicalPreparedFieldState{Inline: &legacyCanonicalPreparedFieldInline{Binding: binding, Pair: json.RawMessage(`["k","v"]`)}}
			current := binding
			switch malformed {
			case "binding":
				current = HashContent([]byte("different-source"))
			case "invalid-binding":
				current, record.Inline.Binding = "invalid", "invalid"
			case "key-ref":
				record.Key.Bytes = 1
			case "value-ref":
				record.Value.Hash = binding
			case "child-keys":
				record.ChildKeys = -1
			case "number":
				record.Inline.Pair = json.RawMessage(`["k",1]`)
			case "nested":
				record.Inline.Pair = json.RawMessage(`["k",[]]`)
			case "key-type":
				record.Inline.Pair = json.RawMessage(`[null,true]`)
			case "arity":
				record.Inline.Pair = json.RawMessage(`["k"]`)
			case "text-limit":
				record.Inline.Pair = json.RawMessage(`["k","` + strings.Repeat("x", 64) + `"]`)
			case "noncanonical":
				record.Inline.Pair = json.RawMessage(`["\u006b","v"]`)
			}
			body, err := json.Marshal(record)
			if err != nil {
				test.Fatal(err)
			}
			if malformed == "unknown" {
				body = bytes.Replace(body, []byte(`"pair":`), []byte(`"extra":true,"pair":`), 1)
			}
			storage := legacyCanonicalPreparedValueTestStorage(test)
			page := append([]byte(`{"v":1,"e":[`), body...)
			page = append(page, ']', '}')
			ref, err := legacyNativeCheckpointWrite(context.Background(), storage.io, page)
			if err != nil {
				test.Fatal(err)
			}
			reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](legacyNativeCheckpointTree{Ref: ref, Count: 1}, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			work := &legacyCanonicalPreparedFieldsRestore{reader: reader, binding: current, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget}
			defer work.close()
			if ready, err := work.step(context.Background()); ready || !errors.Is(err, ErrIntegrity) || work.fields.length() != 0 || work.payload.bytes != 0 {
				test.Fatal("malformed field was admitted", ready, err)
			}
		})
	}
}

func TestLegacyCanonicalPreparedFieldsInlineDenseCAS(test *testing.T) {
	var fields legacyCanonicalPages[legacyCanonicalKeyField]
	expected := make([]legacyCanonicalKeyField, 4096)
	for position := range expected {
		expected[position] = legacyCanonicalKeyField{key: fmt.Sprintf("k%04d", position), value: "x"}
		if err := fields.append(expected[position]); err != nil {
			test.Fatal(err)
		}
	}
	binding := HashContent([]byte("inline-dense-map"))
	storage := legacyCanonicalPreparedValueTestStorage(test)
	tree, writes := legacyCanonicalInlineFieldsWrite(test, &fields, binding, storage, 0, 0, -1)
	got, _ := legacyCanonicalInlineFieldsRestore(test, tree, binding, storage)
	oldStorage := legacyCanonicalPreparedValueTestStorage(test)
	legacyCanonicalInlineFieldsOldWrite(test, expected, binding, oldStorage)
	if !reflect.DeepEqual(got, expected) || writes >= 4096/8 || len(storage.objects) != writes || oldStorage.writes < 4*4096 {
		test.Fatal("dense fields did not reduce child CAS operations", writes, len(storage.objects), oldStorage.writes)
	}
	test.Logf("4096 fields: inline Put calls=%d unique objects=%d; legacy Put calls=%d unique objects=%d", writes, len(storage.objects), oldStorage.writes, len(oldStorage.objects))
}

func TestLegacyCanonicalPreparedFieldsInlineInputHeapAdmission(test *testing.T) {
	binding := HashContent([]byte("inline-field-input-heap"))
	for _, value := range []any{nil, true, "", "\ud55c\U0001f642"} {
		var fields legacyCanonicalPages[legacyCanonicalKeyField]
		if err := fields.append(legacyCanonicalKeyField{key: "key", value: value}); err != nil {
			test.Fatal(err)
		}
		storage := legacyCanonicalPreparedValueTestStorage(test)
		tree, _ := legacyCanonicalInlineFieldsWrite(test, &fields, binding, storage, 0, 0, -1)
		expected := legacyCanonicalPreparedValueTestHeap(test, "key")
		expected.include(legacyCanonicalPreparedValueTestHeap(test, value))
		for _, delta := range []int64{-1, 0} {
			reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](tree, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			if _, found, err := reader.next(context.Background()); err != nil || !found {
				test.Fatal("preload bounded input page", found, err)
			}
			reader.index = 0
			work := &legacyCanonicalPreparedFieldsRestore{reader: reader, binding: binding, store: storage.io}
			retained, err := work.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			work.budget = retained + expected.bytes + legacyNativeAnyCheckpointScratch + delta
			ready, err := work.step(context.Background())
			if ready || delta == 0 && (err != nil || work.payload != expected || work.phase != 5) ||
				delta < 0 && (!errors.Is(err, ErrDocumentResourceLimit) || work.payload.bytes != 0 || work.phase != 0) {
				test.Fatal("inline input heap admission is not exact", value, delta, work.payload, expected, ready, err)
			}
			work.close()
		}
	}
}

func TestLegacyCanonicalPreparedFieldsInlineCASAndAdmission(test *testing.T) {
	ctx := context.Background()
	binding := HashContent([]byte("inline-field-CAS"))
	var fields legacyCanonicalPages[legacyCanonicalKeyField]
	if err := fields.append(legacyCanonicalKeyField{key: "k", value: "v"}); err != nil {
		test.Fatal(err)
	}
	storage := legacyCanonicalPreparedValueTestStorage(test)
	tree, _ := legacyCanonicalInlineFieldsWrite(test, &fields, binding, storage, 0, 0, -1)
	reader, err := newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](tree, storage.io)
	if err != nil {
		test.Fatal(err)
	}
	restore := &legacyCanonicalPreparedFieldsRestore{reader: reader, binding: binding, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget}
	defer restore.close()
	if ready, err := restore.step(ctx); ready || err != nil {
		test.Fatal(ready, err)
	}
	retained, err := restore.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	restore.budget = retained + legacyNativeAnyCheckpointScratch - 1
	if ready, err := restore.step(ctx); ready || !errors.Is(err, ErrDocumentResourceLimit) || restore.fields.length() != 0 {
		test.Fatal("inline restore bypassed heap admission", ready, err)
	}
	type oldField struct {
		Key       legacyNativeCheckpointRef `json:"key"`
		Value     legacyNativeCheckpointRef `json:"value"`
		ChildKeys int                       `json:"child_keys"`
	}
	var oldPage legacyNativeCheckpointListPage[oldField]
	if err := legacyNativeCheckpointReadJSON(ctx, storage.io, tree.Ref, &oldPage); !errors.Is(err, ErrIntegrity) {
		test.Fatal("old reader accepted unsupported inline field", err)
	}
	corrupted := bytes.Clone(storage.objects[tree.Ref.Hash])
	corrupted[len(corrupted)-1] ^= 1
	storage.objects[tree.Ref.Hash] = corrupted
	reader, err = newLegacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState](tree, storage.io)
	if err != nil {
		test.Fatal(err)
	}
	broken := &legacyCanonicalPreparedFieldsRestore{reader: reader, binding: binding, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget}
	defer broken.close()
	if ready, err := broken.step(ctx); ready || !errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("inline restore accepted same-size corrupt CAS bytes", ready, err)
	}
	writer := &legacyCanonicalPreparedFieldsWrite{source: &fields, binding: binding, store: storage.io, budget: legacyCanonicalPreparedValueTestBudget, keySkip: -1}
	defer writer.close()
	if ready, err := writer.step(ctx); ready || err != nil {
		test.Fatal(ready, err)
	}
	if ready, err := writer.step(ctx); ready || !errors.Is(err, ErrIntegrity) || !bytes.Equal(storage.objects[tree.Ref.Hash], corrupted) {
		test.Fatal("inline checkpoint reused corrupt page without a fresh immutable Put", ready, err)
	}
	writer = &legacyCanonicalPreparedFieldsWrite{source: &fields, binding: binding, store: storage.io, budget: legacyNativeAnyCheckpointScratch, keySkip: -1}
	defer writer.close()
	storage.writes = 0
	if ready, err := writer.step(ctx); ready || !errors.Is(err, ErrDocumentResourceLimit) || storage.writes != 0 {
		test.Fatal("inline writer bypassed heap admission", ready, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ready, err := writer.step(canceled); ready || !errors.Is(err, context.Canceled) {
		test.Fatal("inline writer bypassed cancellation", ready, err)
	}
}
