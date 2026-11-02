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

func TestLegacyNativeAnyFieldCheckpointW84FinishedBeforeParent(test *testing.T) {
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "independent", raw: `{"output":{"a":1},"seq":7}`},
		{name: "visible", raw: `{"output":{"old":1},"output":[{"new":2}],"seq":7}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			stop := func(native *legacyNativeObject) bool {
				parser := legacyNativeAnyFieldW84Parser(native, "output")
				return native.containerActive && parser != nil && parser.frame != nil && (fixture.name != "visible" || parser.visible != (legacyNativeAnyValueOwner{}))
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, fixture.raw, nil, stop)
			bundle := legacyNativeObjectRestored{Object: native, Scanner: *scanner, Source: source}
			current, cancel := context.WithCancel(context.Background())
			defer cancel()
			cut := &legacyNativeAnyW78CutContext{Context: current, cancel: cancel, stop: func() bool {
				parser := legacyNativeAnyFieldW84Parser(native, "output")
				return native.containerActive && parser != nil && parser.finished
			}}
			for attempt := 0; attempt < legacyNativeEventW82SourceSteps; attempt++ {
				ready, failure := legacyNativeEventW82Step(test, cut, &bundle, false, false)
				if failure != nil {
					if ready || !errors.Is(failure, context.Canceled) {
						test.Fatal("native finished-before-parent cut changed cancellation", ready, failure)
					}
					break
				}
				if ready || attempt == legacyNativeEventW82SourceSteps-1 {
					test.Fatal("native did not reach finished-before-parent cancellation")
				}
			}
			parser := legacyNativeAnyFieldW84Parser(native, "output")
			if !native.containerActive || parser == nil || !parser.finished {
				test.Fatal("actual native cut did not preserve completed child before parent publication")
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, &bundle.Scanner, request, source, storage)
			for _, boundary := range []string{"parent", "child"} {
				test.Run("reject-mismatched-"+boundary, func(test *testing.T) {
					var invalid legacyNativeObjectCheckpointControl
					if failure := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &invalid); failure != nil {
						test.Fatal(failure)
					}
					if boundary == "parent" {
						invalid.ContainerActive = false
					} else {
						invalid.Fields[native.field].Any.Active = false
					}
					ref, failure := legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), invalid)
					if failure != nil {
						test.Fatal(failure)
					}
					failure = legacyNativeEventW82FenceReject(test, ref, source, request.Binding, legacyNativeCheckpointEventModel, storage.io(), legacyNativeEventW82FenceBudget)
					if !errors.Is(failure, ErrIntegrity) {
						test.Fatal("parent/child publication mismatch lost integrity rejection", failure)
					}
				})
			}
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			if !restored.Object.containerActive || !legacyNativeAnyFieldW84Parser(restored.Object, "output").finished {
				test.Fatal("restore conflated child completion with parent publication")
			}
			if !reflect.DeepEqual(native.destination.Interface().(*wire).Output, restored.Object.destination.Interface().(*wire).Output) {
				test.Fatal("restore published the completed child before the parent resumed")
			}
			if _, failure := legacyNativeEventW82Continue(test, &restored); failure != nil {
				test.Fatal("completed child could not finish parent publication", failure)
			}
			var expected wire
			if failure := json.Unmarshal([]byte(fixture.raw), &expected); failure != nil {
				test.Fatal(failure)
			}
			if !reflect.DeepEqual(expected.Output, restored.Object.destination.Interface().(*wire).Output) {
				test.Fatal("resumed parent did not publish the completed child's exact result")
			}
		})
	}
}

func TestLegacyNativeAnyFieldCheckpointW84EscapedSourceOwnership(test *testing.T) {
	token := "1" + strings.Repeat("0", 2*LegacyNormalizationPartBytes)
	stop := func(native *legacyNativeObject) bool {
		return native.number != nil && native.number.errorText != nil && native.number.errorText.position >= LegacyNormalizationPartBytes && native.number.errorText.position < native.number.errorText.end
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"output":`+token+`}`, &wire{Output: "prior"}, stop)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	_, result := legacyNativeAnyFieldW84FloatRoundTrip(test, request, native, source, storage)
	baseline := legacyNativeAnyCheckpointSourceHeap(source)
	retained := legacyNativeAnyCheckpointSourceHeap(result.Source)
	if baseline.err != nil || retained.err != nil || retained.bytes <= baseline.bytes {
		test.Fatal("escaped diagnostic reader lost its ownership charge after float take", baseline.bytes, retained.bytes)
	}
	cloned, err := legacyNativeObjectCheckpointNestedOwnSource(context.Background(), result.Source)
	if err != nil || legacyNativeAnyCheckpointSourceHeap(cloned).bytes != retained.bytes {
		test.Fatal("source clone dropped the escaped reader ownership charge", err)
	}
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	restored := legacyNativeEventW82FenceRestore(test, request, storage)
	if legacyNativeAnyCheckpointSourceHeap(restored.Source).bytes < retained.bytes {
		test.Fatal("outer object handoff dropped the escaped reader ownership charge")
	}
}

func TestLegacyNativeAnyFieldCheckpointW84FormerlyUnsupported(test *testing.T) {
	fixtures := []struct {
		name   string
		raw    string
		active bool
	}{
		{name: "input-map", raw: `{"input":{"accepted":true}}`},
		{name: "dynamic-map", raw: `{"output":{"accepted":true}}`},
		{name: "dynamic-array", raw: `{"output":["accepted"]}`},
		{name: "dynamic-boolean", raw: `{"output":false}`},
		{name: "dynamic-number", raw: `{"output":17}`},
		{name: "negative-zero", raw: `{"output":-0}`},
		{name: "active-input-parser", raw: `{"input":{"accepted":true}}`, active: true},
		{name: "inactive-input-cache-null", raw: `{"input":{"accepted":true},"input":null}`},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			var stop func(*legacyNativeObject) bool
			if fixture.active {
				stop = func(native *legacyNativeObject) bool { return native.containerActive }
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, fixture.raw, nil, stop)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			if !reflect.DeepEqual(native.destination.Interface(), restored.Object.destination.Interface()) || native.presence != restored.Object.presence || (native.containers == nil) != (restored.Object.containers == nil) {
				test.Fatal("Any field restore changed current value, presence or cache allocation")
			}
			if _, err := legacyNativeEventW82Continue(test, &restored); err != nil {
				test.Fatal("restored actual Event continuation", err)
			}
			var expected wire
			if err := json.Unmarshal([]byte(fixture.raw), &expected); err != nil {
				test.Fatal(err)
			}
			actual := restored.Object.destination.Interface().(*wire)
			actualBody, actualErr := json.Marshal(actual)
			expectedBody, expectedErr := json.Marshal(expected)
			if actualErr != nil || expectedErr != nil || string(actualBody) != string(expectedBody) {
				test.Fatalf("actual Any field differs from native JSON: got %s want %s errors %v %v", actualBody, expectedBody, actualErr, expectedErr)
			}
			if expectedNumber, ok := expected.Output.(float64); ok {
				actualNumber, ok := actual.Output.(float64)
				if !ok || math.Float64bits(actualNumber) != math.Float64bits(expectedNumber) {
					test.Fatal("dynamic number lost its exact float64 bits")
				}
			}
		})
	}
}
