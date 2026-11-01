package domain

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeObjectCheckpointW86InitialBacking(test *testing.T) {
	for _, mode := range []string{"complete", "active-scalar"} {
		test.Run(mode, func(test *testing.T) {
			shared := map[string]any{"kept": true}
			backing := []any{shared, shared, math.Copysign(0, -1), strings.Repeat("hidden-value-", LegacyCanonicalEncodingBytes/3)}
			array := backing[:1]
			initial := &wire{Input: map[string]any{"array": array, "shared": shared}, Output: array}
			raw := `{}`
			var stop func(*legacyNativeObject) bool
			if mode == "active-scalar" {
				raw = `{"id":"` + strings.Repeat("s", 2*LegacyNormalizationPartBytes) + `","input":{"added":true},"seq":5}`
				stop = func(native *legacyNativeObject) bool {
					return native.scalar != nil && native.scalar.position >= LegacyNormalizationPartBytes
				}
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, initial, stop)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			value := restored.Object.destination.Interface().(*wire)
			output := value.Output.([]any)
			stored := value.Input["array"].([]any)
			if len(output) != 1 || cap(output) != len(backing) || reflect.ValueOf(output).Pointer() != reflect.ValueOf(stored).Pointer() {
				test.Fatal("actual Event lost the array view or cross-field backing")
			}
			hidden := output[:cap(output)]
			if reflect.ValueOf(hidden[0]).Pointer() != reflect.ValueOf(hidden[1]).Pointer() || reflect.ValueOf(hidden[1]).Pointer() != reflect.ValueOf(value.Input["shared"]).Pointer() {
				test.Fatal("actual Event lost the visible/hidden shared child")
			}
			if math.Float64bits(hidden[2].(float64)) != math.Float64bits(math.Copysign(0, -1)) || hidden[3] != backing[3] {
				test.Fatal("actual Event changed data outside the logical array length")
			}
			if _, err := legacyNativeEventW82Continue(test, &restored); err != nil {
				test.Fatal("actual Event could not continue after backing restore", err)
			}
			if mode == "active-scalar" && (value.Input["added"] != true || value.Seq != 5) {
				test.Fatal("actual parent continuation lost its normal map update")
			}
			if math.Float64bits(backing[2].(float64)) != math.Float64bits(math.Copysign(0, -1)) || shared["kept"] != true || len(array) != 1 || cap(array) != 4 {
				test.Fatal("capture changed the original backing to evade preservation")
			}
		})
	}
}
