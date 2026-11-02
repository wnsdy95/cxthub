package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeObjectCheckpointW85InitialGraphs(test *testing.T) {
	for _, name := range []string{"same-root", "deep-array", "same-value-separate", "empty-input", "live-id-before-map-update"} {
		test.Run(name, func(test *testing.T) {
			shared := map[string]any{"kept": math.Copysign(0, -1), "nested": []any{true, nil, "original"}}
			initial := &wire{Input: shared, Output: shared}
			raw := `{}`
			var stop func(*legacyNativeObject) bool
			if name == "deep-array" {
				array := make([]any, 2, 4)
				array[0], array[1] = shared, shared
				initial = &wire{Input: map[string]any{"array": array, "again": shared}, Output: array}
			} else if name == "same-value-separate" {
				initial.Output = map[string]any{"kept": math.Copysign(0, -1), "nested": []any{true, nil, "original"}}
			} else if name == "empty-input" {
				initial = &wire{Input: map[string]any{}}
			} else if name == "live-id-before-map-update" {
				raw = `{"id":"` + strings.Repeat("z", 2*LegacyNormalizationPartBytes) + `","input":{"added":true},"seq":9}`
				stop = func(native *legacyNativeObject) bool {
					return native.scalar != nil && native.scalar.position >= LegacyNormalizationPartBytes
				}
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, initial, stop)
			before, err := json.Marshal(native.destination.Interface())
			if err != nil {
				test.Fatal(err)
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			value := restored.Object.destination.Interface().(*wire)
			after, err := json.Marshal(value)
			if err != nil || string(before) != string(after) {
				test.Fatal("initial graph checkpoint changed actual parent values", err)
			}
			if name == "deep-array" {
				array := value.Output.([]any)
				stored := value.Input["array"].([]any)
				if len(array) != 2 || cap(array) != 4 || reflect.ValueOf(array).Pointer() != reflect.ValueOf(stored).Pointer() || reflect.ValueOf(array[0]).Pointer() != reflect.ValueOf(array[1]).Pointer() || reflect.ValueOf(array[0]).Pointer() != reflect.ValueOf(value.Input["again"]).Pointer() {
					test.Fatal("initial graph lost deep cross-field identity or array geometry")
				}
			} else if name != "empty-input" {
				alias := reflect.ValueOf(value.Input).Pointer() == reflect.ValueOf(value.Output).Pointer()
				if alias != (name != "same-value-separate") {
					test.Fatal("initial graph merged equal values or split a shared source root")
				}
			}
			if _, err := legacyNativeEventW82Continue(test, &restored); err != nil {
				test.Fatal("actual parent could not continue after initial graph restore", err)
			}
			if name == "live-id-before-map-update" && (value.Input["added"] != true || value.Output.(map[string]any)["added"] != true || value.Seq != 9) {
				test.Fatal("initial graph alias did not follow the real map decoder after restart")
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointW85InitialGraphFences(test *testing.T) {
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Input: map[string]any{"kept": true}, Output: []any{float64(7)}}, nil)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	for _, name := range []string{"count", "duplicate-slot", "ref", "field-type", "wrong-binding"} {
		test.Run(name, func(test *testing.T) {
			var state legacyNativeObjectCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
				test.Fatal(err)
			}
			input, output := native.metadata.match("input"), native.metadata.match("output")
			switch name {
			case "count":
				state.InitialGraphCount++
			case "duplicate-slot":
				state.Fields[output].InitialGraph = state.Fields[input].InitialGraph
			case "ref":
				state.InitialGraphs.Hash = HashContent([]byte("unknown initial graph"))
			case "field-type":
				state.Fields[native.metadata.match("seq")].InitialGraph = state.Fields[input].InitialGraph
				state.Fields[input].InitialGraph = 0
			case "wrong-binding":
				state.Binding = HashContent([]byte("different initial graph parent"))
			}
			ref, err := legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), state)
			if err != nil {
				test.Fatal(err)
			}
			err = legacyNativeEventW82FenceReject(test, ref, source, request.Binding, legacyNativeCheckpointEventModel, storage.io(), legacyNativeEventW82FenceBudget)
			if !errors.Is(err, ErrIntegrity) && name != "ref" {
				test.Fatal("initial graph parent fence did not reject malformed state", name, err)
			}
			if err == nil {
				test.Fatal("unknown initial graph reference became ready")
			}
		})
	}
}
