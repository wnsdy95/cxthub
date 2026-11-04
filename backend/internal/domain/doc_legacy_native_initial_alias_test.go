package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeInitialAliasW91Diamond() *wire {
	child := map[string]any{"old": strings.Repeat("old child", 2048)}
	shared := map[string]any{"kept": true, "child": child}
	diamond := map[string]any{"input": shared}
	backing := []any{diamond, shared, diamond}
	return &wire{Input: shared, Output: map[string]any{"paths": backing[:1:3], "separate": child}}
}

func legacyNativeInitialAliasW91Restore(test *testing.T, raw string, initial *wire) legacyNativeObjectRestored {
	test.Helper()
	stop := func(native *legacyNativeObject) bool {
		return native.field == native.metadata.match("id") && native.phase == legacyNativeObjectAfter
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, initial, stop)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	return legacyNativeEventW82FenceRestore(test, request, storage)
}

func TestLegacyNativeObjectCheckpointW91InitialAliasTransitions(test *testing.T) {
	for _, fixture := range []struct {
		name string
		tail string
		live bool
	}{
		{name: "grow-through-diamond-and-hidden-tail", tail: `,"input":{"added":"new"},"seq":9}`, live: true},
		{name: "shrink-preserves-separate-old-child", tail: `,"input":{"child":null},"seq":9}`, live: true},
		{name: "equal-size-value-change", tail: `,"input":{"kept":false},"seq":9}`, live: true},
		{name: "wrong-input-then-merge", tail: `,"input":[],"input":{"added":true},"seq":9}`, live: true},
		{name: "rejected-output-keeps-alias", tail: `,"output":1e999,"input":{"added":true},"seq":9}`, live: true},
		{name: "input-null-detaches-only-input", tail: `,"input":null,"input":{"fresh":true},"seq":9}`},
		{name: "output-null", tail: `,"output":null,"input":{"added":true},"seq":9}`},
		{name: "output-container", tail: `,"output":[{"fresh":true}],"input":{"added":true},"seq":9}`},
		{name: "output-long-string", tail: `,"output":"` + strings.Repeat("s", LegacyCanonicalEncodingBytes+1) + `","input":{"added":true},"seq":9}`},
		{name: "output-long-number", tail: `,"output":0.` + strings.Repeat("0", LegacyCanonicalEncodingBytes) + `1,"input":{"added":true},"seq":9}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `{"id":"` + strings.Repeat(`\u007a`, LegacyNormalizationPartBytes/4+32) + `"` + fixture.tail
			expected := legacyNativeInitialAliasW91Diamond()
			wantErr := json.Unmarshal([]byte(raw), expected)
			initial := legacyNativeInitialAliasW91Diamond()
			restored := legacyNativeInitialAliasW91Restore(test, raw, initial)
			if restored.Object.initialAlias.count != 3 {
				test.Fatal("actual validated graph did not preserve occurrence multiplicity", restored.Object.initialAlias.count)
			}
			_, err := legacyNativeEventW82Continue(test, &restored)
			var actualType, expectedType *json.UnmarshalTypeError
			if (err == nil) != (wantErr == nil) || err != nil && (!errors.As(err, &actualType) || !errors.As(wantErr, &expectedType)) {
				test.Fatal("normal alias continuation changed decoder errors", err, wantErr)
			}
			actual := restored.Object.destination.Interface().(*wire)
			if !reflect.DeepEqual(actual, expected) {
				test.Fatal("normal alias continuation differs from ordinary JSON", fixture.name)
			}
			if fixture.live {
				if restored.Object.initialAlias.count != 3 {
					test.Fatal("an unchanged published binding lost its live alias count")
				}
				legacyNativeAliasW91Accounting(test, restored.Object, fixture.name)
				output := actual.Output.(map[string]any)
				array := output["paths"].([]any)
				backing := array[:cap(array)]
				if reflect.ValueOf(backing[1]).Pointer() != reflect.ValueOf(actual.Input).Pointer() ||
					reflect.ValueOf(backing[0].(map[string]any)["input"]).Pointer() != reflect.ValueOf(actual.Input).Pointer() ||
					reflect.ValueOf(backing[2].(map[string]any)["input"]).Pointer() != reflect.ValueOf(actual.Input).Pointer() {
					test.Fatal("normal mutation split a diamond or hidden-tail alias")
				}
			} else if restored.Object.initialAlias.count != 0 {
				test.Fatal("replaced root retained a stale alias binding")
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointW91AliasCountQuantum(test *testing.T) {
	shared := map[string]any{"kept": true}
	diamond := map[string]any{"input": shared}
	const fanout = 2400
	array := make([]any, fanout)
	for index := range array {
		array[index] = diamond
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Input: shared, Output: array}, nil)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	steps := 0
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		checking := work.phase == 7 && work.aliasTask != nil
		writes, reads := storage.writes, len(storage.dependencies)
		var before int64
		if checking {
			before = work.aliasTask.nodes
		}
		ready, err := work.step(context.Background())
		if err != nil {
			test.Fatal("actual bounded alias count failed", err)
		}
		if checking {
			steps++
			if writes != storage.writes || reads != len(storage.dependencies) || work.aliasTask != nil && work.aliasTask.nodes-before > 4 {
				test.Fatal("alias count exceeded its Step allocation or IO quantum")
			}
			if work.aliasTask != nil {
				legacyNativeInitialAliasW91Metadata(test, work.aliasTask)
			}
		}
		if ready {
			if steps < fanout/4 || work.native.initialAlias.count != fanout {
				test.Fatal("alias fanout was lost or hidden in one Step", steps, work.native.initialAlias.count)
			}
			if _, err := work.take(context.Background()); err != nil {
				test.Fatal(err)
			}
			return
		}
	}
	test.Fatal("alias count exceeded the unchanged restore schedule")
}

func legacyNativeInitialAliasW91Metadata(test *testing.T, work *legacyNativeInitialAliasPreparation) {
	test.Helper()
	var required docRangeHeaderHeapEstimate
	required.allocation(1, int64(unsafe.Sizeof(*work)))
	for node := work.first; node != nil; node = node.next {
		required.allocation(1, int64(unsafe.Sizeof(*node)))
		if node.cursor != nil {
			required.allocation(1, int64(unsafe.Sizeof(*node.cursor)))
		}
	}
	actual, err := work.retainedBytes()
	if err != nil || required.err != nil || actual != required.bytes {
		test.Fatal("actual scratch ownership omitted per-allocation overhead", actual, required.bytes, err, required.err)
	}
}

func TestLegacyNativeObjectCheckpointW91AliasCountFences(test *testing.T) {
	for _, name := range []string{"cancel", "aggregate-budget", "close"} {
		test.Run(name, func(test *testing.T) {
			shared := map[string]any{"kept": true}
			diamond := map[string]any{"input": shared, "spare": true}
			array := make([]any, 32)
			for index := range array {
				array[index] = diamond
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Input: shared, Output: array}, nil)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeEventW82FenceBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
				if work.aliasTask != nil && work.aliasTask.nodes >= 2 {
					break
				}
				ready, err := work.step(context.Background())
				if err != nil || ready {
					test.Fatal("restore missed its real alias preparation boundary", ready, err)
				}
			}
			preparation, restored := work.aliasTask, work.native
			if preparation == nil || preparation.ready || preparation.nodes < 2 || restored.initialAlias.count != 0 {
				test.Fatal("fixture lacks a real unpublished alias preparation")
			}
			legacyNativeInitialAliasW91Metadata(test, preparation)
			writes, reads := storage.writes, len(storage.dependencies)
			if name == "close" {
				work.close()
			} else {
				current, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := ErrDocumentResourceLimit
				if name == "cancel" {
					cancel()
					want = context.Canceled
				} else {
					retained, err := work.retainedBytes()
					if err != nil {
						test.Fatal(err)
					}
					work.budget = retained
				}
				if ready, err := work.step(current); ready || !errors.Is(err, want) {
					test.Fatal("alias preparation ignored cancellation or current aggregate budget", ready, err)
				}
			}
			retained, err := preparation.retainedBytes()
			if !work.closed || !preparation.closed || retained != 0 || err != nil || restored.initialAlias.count != 0 || storage.writes != writes || len(storage.dependencies) != reads {
				test.Fatal("failed alias preparation retained resources, performed IO, or published a partial binding", retained, err)
			}
		})
	}
}

func TestLegacyNativeInitialAliasW91CheckedReplacement(test *testing.T) {
	for _, fixture := range []struct {
		name                                 string
		current, previous, next, count, want int64
		valid                                bool
	}{
		{name: "grow", current: 100, previous: 10, next: 20, count: 3, want: 130, valid: true},
		{name: "shrink", current: 100, previous: 20, next: 10, count: 3, want: 70, valid: true},
		{name: "equal", current: 100, previous: 20, next: 20, count: 3, want: 100, valid: true},
		{name: "zero", current: 100, previous: math.MaxInt64, next: math.MaxInt64, count: 0, want: 100, valid: true},
		{name: "negative-count", current: 100, previous: 1, next: 1, count: -1},
		{name: "old-product-overflow", current: math.MaxInt64, previous: math.MaxInt64, next: 1, count: 2},
		{name: "new-product-overflow", current: math.MaxInt64, previous: 1, next: math.MaxInt64, count: 2},
		{name: "underflow", current: 1, previous: 2, next: 1, count: 1},
		{name: "addition-overflow", current: math.MaxInt64, previous: 0, next: 1, count: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			actual, err := legacyNativeInitialAliasReplace(fixture.current, fixture.previous, fixture.next, fixture.count)
			if fixture.valid && (err != nil || actual != fixture.want) || !fixture.valid && !errors.Is(err, ErrIntegrity) {
				test.Fatal("checked alias accounting returned the wrong result", actual, err)
			}
		})
	}
}
