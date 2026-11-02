package domain

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNativeContainerCheckpointActualMapEntries(test *testing.T) {
	var builder strings.Builder
	builder.Grow(256)
	builder.WriteString("accepted \ud55c\uae00")
	text := builder.String()
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: 1 << 20, Nodes: 64}
	for _, fixture := range []struct {
		name  string
		value any
		extra int64
	}{
		{name: "null"},
		{name: "false", value: false},
		{name: "true", value: true},
		{name: "negative_zero", value: math.Float64frombits(1 << 63)},
		{name: "maximum_float", value: math.MaxFloat64},
		{name: "text", value: text, extra: 2 * int64(builder.Cap()-len(text))},
		{name: "nil_array", value: []any(nil)},
		{name: "empty_array", value: []any{}},
		{name: "array", value: []any{nil, true}},
		{name: "nil_object", value: map[string]any(nil)},
		{name: "empty_object", value: map[string]any{}},
		{name: "object", value: map[string]any{"nested": []any{"text", false}}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			native := newLegacyNativeAnyMap()
			expectedEntry := legacyNativeAnyMapCheckpointOraclePut(test, native, "", 64, fixture.value, fixture.extra)
			cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, false)
			entry, present, err := cursor.next(context.Background())
			if err != nil || !present || !reflect.DeepEqual(entry, expectedEntry) {
				test.Fatal("actual ownership cursor changed the accepted entry", entry, err)
			}
			if _, present, err := cursor.next(context.Background()); present || err != nil {
				test.Fatal("actual ownership cursor lost its exact EOF", present, err)
			}
			var reference uint64
			switch typed := fixture.value.(type) {
			case string:
				reference = 64
			case []any:
				if typed != nil {
					reference = 1
				}
			case map[string]any:
				if typed != nil {
					reference = 1
				}
			}
			snapshot, err := entry.checkpoint(context.Background(), 0, reference, bounds)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(snapshot.close)
			body, err := snapshot.encode(context.Background(), bounds)
			if err != nil || len(body) != legacyNativeMapCheckpointEntryBytes {
				test.Fatal("actual cursor entry could not produce a bounded typed row", len(body), err)
			}
			record, err := decodeLegacyNativeMapCheckpointEntry(context.Background(), body, bounds)
			if err != nil || record.Key.Kind != legacyNativeCheckpointString || record.Key.Length != 0 || record.Key.Capacity != 64 || record.Value.Reference != reference {
				test.Fatal("typed row changed canonical key capacity or value reference", record, err)
			}
			inline, present, err := record.Value.inline(context.Background(), bounds)
			if err != nil {
				test.Fatal(err)
			}
			if present && !reflect.DeepEqual(inline, fixture.value) {
				test.Fatal("typed row changed inline value or typed nil", inline, fixture.value)
			}
			if typed, ok := fixture.value.(float64); ok && (!present || math.Float64bits(inline.(float64)) != math.Float64bits(typed)) {
				test.Fatal("typed row lost native numeric bits")
			}
			if fixture.name == "empty_array" || fixture.name == "empty_object" {
				if present || record.Value.Nil {
					test.Fatal("empty referenced container became an inline nil")
				}
			}
			if fixture.name == "text" {
				payload, err := snapshot.value.read(context.Background(), 0, len(text))
				if err != nil || string(payload) != text || record.Value.Capacity != uint64(builder.Cap()) || present {
					test.Fatal("actual string capacity or accepted bytes changed", record.Value, err)
				}
			}
			var expected docRangeHeaderHeapEstimate
			expected.allocation(1, int64(unsafe.Sizeof(*snapshot)))
			for _, child := range []*legacyNativeValueSnapshot{snapshot.key, snapshot.value} {
				expected.allocation(1, int64(unsafe.Sizeof(*child)))
				if child.state.Kind == legacyNativeCheckpointString {
					expected.allocation(int64(child.state.Capacity), 1)
				}
			}
			retained, err := snapshot.retainedBytes()
			if err != nil || expected.err != nil || retained != expected.bytes {
				test.Fatal("typed row omitted snapshot metadata or retained a referenced graph", retained, expected, err)
			}
			snapshot.close()
			if retained, err := snapshot.retainedBytes(); retained != 0 || err != nil {
				test.Fatal("closed row retained metadata or text", retained, err)
			}
			if _, err := snapshot.encode(context.Background(), bounds); !errors.Is(err, ErrIntegrity) {
				test.Fatal("closed row was encoded", err)
			}
		})
	}
}

func TestLegacyNativeContainerCheckpointRolesAndBounds(test *testing.T) {
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: 16, Nodes: 4}
	value := legacyNativeValueCheckpoint{Version: 1, Kind: legacyNativeCheckpointNull}
	row := legacyNativeArrayCheckpointEntry{Position: 2, Value: value}
	body, err := row.encode(context.Background(), bounds, 3)
	if err != nil || len(body) != legacyNativeArrayCheckpointEntryBytes {
		test.Fatal(err)
	}
	decoded, err := decodeLegacyNativeArrayCheckpointEntry(context.Background(), body, bounds, 3)
	if err != nil || decoded != row {
		test.Fatal("array position or value changed", decoded, err)
	}
	for _, length := range []uint64{0, 1, 2, 5, math.MaxUint64} {
		if _, err := row.encode(context.Background(), bounds, length); !errors.Is(err, ErrIntegrity) {
			test.Fatal("array row escaped its declared container length", length, err)
		}
		if decoded, err := decodeLegacyNativeArrayCheckpointEntry(context.Background(), body, bounds, length); decoded != (legacyNativeArrayCheckpointEntry{}) || !errors.Is(err, ErrIntegrity) {
			test.Fatal("array decoder accepted a wrong declared length", length, decoded, err)
		}
	}
	for length := 0; length < len(body); length++ {
		if _, err := decodeLegacyNativeArrayCheckpointEntry(context.Background(), body[:length], bounds, 3); !errors.Is(err, ErrIntegrity) {
			test.Fatal("truncated array row accepted", length, err)
		}
	}
	binary.LittleEndian.PutUint64(body[:8], math.MaxUint64)
	if _, err := decodeLegacyNativeArrayCheckpointEntry(context.Background(), body, bounds, 3); !errors.Is(err, ErrIntegrity) {
		test.Fatal("overflowing position accepted", err)
	}
	pair := legacyNativeMapCheckpointEntry{Key: legacyNativeValueCheckpoint{Version: 1, Kind: legacyNativeCheckpointString}, Value: value}
	body, err = pair.encode(context.Background(), bounds)
	if err != nil {
		test.Fatal(err)
	}
	body[1] = legacyNativeCheckpointBool
	if decoded, err := decodeLegacyNativeMapCheckpointEntry(context.Background(), body, bounds); decoded != (legacyNativeMapCheckpointEntry{}) || !errors.Is(err, ErrIntegrity) {
		test.Fatal("map row accepted a nontext key", decoded, err)
	}
	entry := legacyNativeAnyMapCheckpointEntry{Key: "key", KeyCapacity: 3, Value: "text", ValueBytes: 100, ValueExcess: 1}
	if snapshot, err := entry.checkpoint(context.Background(), 0, 3, bounds); snapshot != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("odd actual string excess was guessed as capacity", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := decodeLegacyNativeArrayCheckpointEntry(ctx, nil, bounds, 0); !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation lost precedence over malformed array row", err)
	}
	if _, err := decodeLegacyNativeMapCheckpointEntry(ctx, nil, bounds); !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation lost precedence over malformed map row", err)
	}
}
