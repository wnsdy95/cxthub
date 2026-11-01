package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeNestedCheckpointW83InitializationAtomic(test *testing.T) {
	for _, name := range []string{"locked", "provider_metadata"} {
		test.Run(name, func(test *testing.T) {
			raw := `{"` + name + `":{}}`
			request, source := legacyNativeAnyW78FullSource(test, raw)
			value := &wire{}
			native, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEventNativeFields, 3, true)
			if err != nil {
				test.Fatal(err)
			}
			native.start(0, len(raw))
			native.field, native.phase = native.metadata.match(name), legacyNativeObjectApply
			native.presence = eventPresenceName(name)
			native.valueStart, native.valueEnd = strings.Index(raw, "{}"), len(raw)
			native.position, native.valueFirst, native.valueStarted = native.valueStart, '{', true
			var scanner legacyJSONValueScanner
			if err := scanner.reset(native.parentDepth); err != nil {
				test.Fatal(err)
			}
			current, cancel := context.WithCancel(context.Background())
			cancel()
			input := &legacyNormalizationInput{ctx: current, source: source}
			if complete, err := native.decodeNestedObject(current, input, &scanner); complete || !errors.Is(err, context.Canceled) {
				test.Fatal("child initialization lost cancellation", complete, err)
			}
			field := native.destination.Elem().Field(native.metadata.fields[native.field].index)
			if !field.IsNil() || native.nestedActive || native.nested[native.field] != nil || native.heaps[native.field].bytes != 0 {
				test.Fatal("canceled initialization published an unowned partial destination")
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, &scanner, request, source, storage)
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			if _, err := legacyNativeNestedW83Continue(test, &restored); err != nil {
				test.Fatal("canceled initialization could not checkpoint and retry", err)
			}
			if restored.Object.destination.Elem().Field(restored.Object.metadata.fields[restored.Object.metadata.match(name)].index).IsNil() {
				test.Fatal("fresh-context retry did not allocate the actual child")
			}
		})
	}
}
