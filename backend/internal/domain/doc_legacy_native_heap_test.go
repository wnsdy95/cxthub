package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeHeapString string

type legacyNativeHeapStrings []legacyNativeHeapString

type legacyNativeHeapInteger int64

func TestLegacyNativeHeapEmptyAndNilStorage(test *testing.T) {
	var emptyInterface any
	var emptyPointer *string
	var emptySlice []string
	var emptyMap map[string]any
	values := []struct {
		name  string
		value reflect.Value
	}{
		{"invalid", reflect.Value{}},
		{"interface", reflect.ValueOf(&emptyInterface).Elem()},
		{"pointer", reflect.ValueOf(emptyPointer)},
		{"slice", reflect.ValueOf(emptySlice)},
		{"map", reflect.ValueOf(emptyMap)},
		{"empty_slice", reflect.ValueOf([]string{})},
		{"empty_struct", reflect.ValueOf(struct{}{})},
		{"zero_width_slice", reflect.ValueOf(make([]struct{}, 0, 3))},
	}
	for _, fixture := range values {
		test.Run(fixture.name, func(test *testing.T) {
			var expected docRangeHeaderHeapEstimate
			if fixture.value.IsValid() && fixture.value.Type().Size() != 0 {
				expected.allocation(1, int64(fixture.value.Type().Size()))
			}
		})
	}
	value := reflect.ValueOf(map[string]any{})
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(value.Type().Size()))
	expected.allocation(1, 64)
	expected.allocation(8, int64(value.Type().Key().Size()+value.Type().Elem().Size())+8)
}

func TestLegacyNativeHeapPrimitiveKindsAndAliases(test *testing.T) {
	for _, value := range []any{
		true, int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1), uintptr(1),
		float32(1.5), float64(1.5), legacyNativeHeapInteger(7),
		"text", legacyNativeHeapString("alias"), EventMessage, RoleUser,
		ProviderKind("codex"), FidelityTier("tier"), ContentHash("hash"), json.Number("123.450e-8"),
	} {
		reflected := reflect.ValueOf(value)
		test.Run(reflected.Type().String(), func(test *testing.T) {
			var expected docRangeHeaderHeapEstimate
			expected.allocation(1, int64(reflected.Type().Size()))
			if reflected.Kind() == reflect.String {
				expected.allocation(int64(reflected.Len()), 1)
			}
		})
	}
	stringsAlias := legacyNativeHeapStrings{"first", "backing-only"}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(stringsAlias)))
	expected.allocation(int64(cap(stringsAlias)), int64(unsafe.Sizeof(legacyNativeHeapString(""))))
	for _, text := range stringsAlias {
		expected.allocation(int64(len(text)), 1)
	}
}

func TestLegacyNativeHeapEnvelopeSourceModelsDuplicateTail(test *testing.T) {
	retained := strings.Repeat("old-model", 512)
	var envelope CIREnvelope
	raw := []byte(fmt.Sprintf(`{"source_models":["first",%q,"last"],"source_models":[null]}`, retained))
	if err := json.Unmarshal(raw, &envelope); err != nil {
		test.Fatal(err)
	}
	if len(envelope.SourceModels) != 1 || cap(envelope.SourceModels) < 3 || envelope.SourceModels[0] != "first" {
		test.Fatal("source model fixture lost duplicate-key retention")
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(envelope)))
	expected.allocation(int64(cap(envelope.SourceModels)), int64(unsafe.Sizeof("")))
	for _, text := range envelope.SourceModels[:cap(envelope.SourceModels)] {
		expected.allocation(int64(len(text)), 1)
	}
	envelope.SourceModels = envelope.SourceModels[:0]
	if err := json.Unmarshal([]byte(`{"source_models":null}`), &envelope); err != nil {
		test.Fatal(err)
	}
	if envelope.SourceModels != nil {
		test.Fatal("source model null kept a backing slice")
	}
	expected = docRangeHeaderHeapEstimate{}
	expected.allocation(1, int64(unsafe.Sizeof(envelope)))
}

type legacyNativeHeapCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *legacyNativeHeapCancelContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
