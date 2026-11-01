package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"testing"
	"time"
)

type legacyNativeValueWireOracleFixture struct {
	name   string
	state  legacyNativeValueCheckpoint
	body   [40]byte
	inline any
	ready  bool
}

func legacyNativeValueWireOracleBounds() legacyNativeValueCheckpointBounds {
	return legacyNativeValueCheckpointBounds{
		PayloadBytes: uint64(MaxFinalizedDocBytes)*6 + uint64(len(legacyNativeTypedNumberErrorPrefix)+len(legacyNativeTypedNumberErrorSuffix)+2),
		Nodes:        uint64(MaxFinalizedDocBytes),
	}
}

func legacyNativeValueWireOracleBody(state legacyNativeValueCheckpoint) [40]byte {
	var body [40]byte
	body[0], body[1] = state.Version, state.Kind
	if state.Nil {
		body[2] = 1
	}
	for ordinal, word := range []uint64{state.Bits, state.Reference, state.Length, state.Capacity} {
		for octet := 0; octet < 8; octet++ {
			body[8+ordinal*8+octet] = byte(word >> (8 * octet))
		}
	}
	return body
}

func legacyNativeValueWireOracleFixtures() []legacyNativeValueWireOracleFixture {
	return []legacyNativeValueWireOracleFixture{
		{
			name: "null", state: legacyNativeValueCheckpoint{Version: 1, Kind: 0},
			body: [40]byte{0: 1}, ready: true,
		},
		{
			name: "false", state: legacyNativeValueCheckpoint{Version: 1, Kind: 1},
			body: [40]byte{0: 1, 1: 1}, inline: false, ready: true,
		},
		{
			name: "true", state: legacyNativeValueCheckpoint{Version: 1, Kind: 1, Bits: 1},
			body: [40]byte{0: 1, 1: 1, 8: 1}, inline: true, ready: true,
		},
		{
			name: "positive_zero", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2},
			body: [40]byte{0: 1, 1: 2}, inline: float64(0), ready: true,
		},
		{
			name: "negative_zero", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0x8000000000000000},
			body: [40]byte{0: 1, 1: 2, 15: 0x80}, inline: math.Float64frombits(0x8000000000000000), ready: true,
		},
		{
			name: "positive_subnormal", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 1},
			body: [40]byte{0: 1, 1: 2, 8: 1}, inline: math.SmallestNonzeroFloat64, ready: true,
		},
		{
			name: "negative_subnormal", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0x8000000000000001},
			body: [40]byte{0: 1, 1: 2, 8: 1, 15: 0x80}, inline: -math.SmallestNonzeroFloat64, ready: true,
		},
		{
			name: "minimum_normal", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0x0010000000000000},
			body: [40]byte{0: 1, 1: 2, 14: 0x10}, inline: math.Float64frombits(0x0010000000000000), ready: true,
		},
		{
			name: "nonuniform_finite_word", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0x3ff1020304050607},
			body:   [40]byte{0: 1, 1: 2, 8: 7, 9: 6, 10: 5, 11: 4, 12: 3, 13: 2, 14: 0xf1, 15: 0x3f},
			inline: math.Float64frombits(0x3ff1020304050607), ready: true,
		},
		{
			name: "maximum_finite", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0x7fefffffffffffff},
			body:   [40]byte{0: 1, 1: 2, 8: 0xff, 9: 0xff, 10: 0xff, 11: 0xff, 12: 0xff, 13: 0xff, 14: 0xef, 15: 0x7f},
			inline: math.MaxFloat64, ready: true,
		},
		{
			name: "negative_maximum_finite", state: legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: 0xffefffffffffffff},
			body:   [40]byte{0: 1, 1: 2, 8: 0xff, 9: 0xff, 10: 0xff, 11: 0xff, 12: 0xff, 13: 0xff, 14: 0xef, 15: 0xff},
			inline: -math.MaxFloat64, ready: true,
		},
		{
			name: "empty_string", state: legacyNativeValueCheckpoint{Version: 1, Kind: 3},
			body: [40]byte{0: 1, 1: 3},
		},
		{
			name: "empty_string_offset_spare_capacity", state: legacyNativeValueCheckpoint{Version: 1, Kind: 3, Reference: 17, Capacity: 31},
			body: [40]byte{0: 1, 1: 3, 16: 17, 32: 31},
		},
		{
			name: "string_byte_offset_and_physical_capacity",
			state: legacyNativeValueCheckpoint{
				Version: 1, Kind: 3, Reference: 0x01020304, Length: 0x05060708, Capacity: 0x090a0b0c,
			},
			body: [40]byte{0: 1, 1: 3, 16: 4, 17: 3, 18: 2, 19: 1, 24: 8, 25: 7, 26: 6, 27: 5, 32: 12, 33: 11, 34: 10, 35: 9},
		},
		{
			name: "nil_array", state: legacyNativeValueCheckpoint{Version: 1, Kind: 4, Nil: true},
			body: [40]byte{0: 1, 1: 4, 2: 1}, inline: []any(nil), ready: true,
		},
		{
			name: "empty_array", state: legacyNativeValueCheckpoint{Version: 1, Kind: 4, Reference: 1},
			body: [40]byte{0: 1, 1: 4, 16: 1},
		},
		{
			name: "empty_array_spare_capacity", state: legacyNativeValueCheckpoint{Version: 1, Kind: 4, Reference: 7, Capacity: 19},
			body: [40]byte{0: 1, 1: 4, 16: 7, 32: 19},
		},
		{
			name: "array_logical_slot_and_physical_capacity",
			state: legacyNativeValueCheckpoint{
				Version: 1, Kind: 4, Reference: 0x01020304, Length: 0x01020305, Capacity: 0x02030405,
			},
			body: [40]byte{0: 1, 1: 4, 16: 4, 17: 3, 18: 2, 19: 1, 24: 5, 25: 3, 26: 2, 27: 1, 32: 5, 33: 4, 34: 3, 35: 2},
		},
		{
			name: "nil_object", state: legacyNativeValueCheckpoint{Version: 1, Kind: 5, Nil: true},
			body: [40]byte{0: 1, 1: 5, 2: 1}, inline: map[string]any(nil), ready: true,
		},
		{
			name: "empty_object", state: legacyNativeValueCheckpoint{Version: 1, Kind: 5, Reference: 1},
			body: [40]byte{0: 1, 1: 5, 16: 1},
		},
		{
			name: "object_logical_slot_and_count", state: legacyNativeValueCheckpoint{Version: 1, Kind: 5, Reference: 0x01020304, Length: 0x01020305},
			body: [40]byte{0: 1, 1: 5, 16: 4, 17: 3, 18: 2, 19: 1, 24: 5, 25: 3, 26: 2, 27: 1},
		},
	}
}

func legacyNativeValueWireOracleDecodeRejected(test *testing.T, body []byte, bounds legacyNativeValueCheckpointBounds) {
	test.Helper()
	before := append([]byte(nil), body...)
	state, err := decodeLegacyNativeValueCheckpoint(context.Background(), body, bounds)
	if !errors.Is(err, ErrIntegrity) || state != (legacyNativeValueCheckpoint{}) {
		test.Fatalf("rejected body=%x bounds=%+v: state=%+v error=%v", before, bounds, state, err)
	}
	if !bytes.Equal(body, before) {
		test.Fatal("decoder modified rejected input")
	}
}

func legacyNativeValueWireOracleCheck(test *testing.T, state legacyNativeValueCheckpoint, bounds legacyNativeValueCheckpointBounds, accepted bool) {
	test.Helper()
	validation := state.validate(bounds)
	body, encoding := state.encode(context.Background(), bounds)
	oracle := legacyNativeValueWireOracleBody(state)
	before := oracle
	decoded, decoding := decodeLegacyNativeValueCheckpoint(context.Background(), oracle[:], bounds)
	if oracle != before {
		test.Fatal("decoder modified oracle input")
	}
	if accepted {
		if validation != nil || encoding != nil || decoding != nil || decoded != state || !bytes.Equal(body, oracle[:]) {
			test.Fatalf("accepted state=%+v bounds=%+v: validation=%v encoding=%v decoding=%v decoded=%+v body=%x", state, bounds, validation, encoding, decoding, decoded, body)
		}
		return
	}
	if !errors.Is(validation, ErrIntegrity) || !errors.Is(encoding, ErrIntegrity) || body != nil ||
		!errors.Is(decoding, ErrIntegrity) || decoded != (legacyNativeValueCheckpoint{}) {
		test.Fatalf("rejected state=%+v bounds=%+v: validation=%v encoding=%v body=%x decoding=%v decoded=%+v", state, bounds, validation, encoding, body, decoding, decoded)
	}
	value, ready, err := state.inline(context.Background(), bounds)
	if value != nil || ready || !errors.Is(err, ErrIntegrity) {
		test.Fatalf("invalid inline state=%+v bounds=%+v: value=%#v ready=%v error=%v", state, bounds, value, ready, err)
	}
}

func TestLegacyNativeValueCheckpointWireGolden(test *testing.T) {
	bounds := legacyNativeValueWireOracleBounds()
	if legacyNativeValueCheckpointBytes != 40 || legacyNativeCheckpointNull != 0 || legacyNativeCheckpointBool != 1 ||
		legacyNativeCheckpointFloat64 != 2 || legacyNativeCheckpointString != 3 || legacyNativeCheckpointArray != 4 || legacyNativeCheckpointObject != 5 {
		test.Fatal("frozen V1 width or kind numbers changed")
	}
	for _, fixture := range legacyNativeValueWireOracleFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			if oracle := legacyNativeValueWireOracleBody(fixture.state); oracle != fixture.body {
				test.Fatalf("independent byte oracle=%x literal golden=%x", oracle, fixture.body)
			}
			legacyNativeValueWireOracleCheck(test, fixture.state, bounds, true)
			encoded, err := fixture.state.encode(context.Background(), bounds)
			if err != nil || len(encoded) != 40 || !bytes.Equal(encoded, fixture.body[:]) {
				test.Fatalf("fixed little-endian golden: body=%x error=%v", encoded, err)
			}
			guarded := append([]byte{0xa5}, fixture.body[:]...)
			guarded = append(guarded, 0x5a)
			before := append([]byte(nil), guarded...)
			decoded, err := decodeLegacyNativeValueCheckpoint(context.Background(), guarded[1:41], bounds)
			if err != nil || decoded != fixture.state || !bytes.Equal(guarded, before) {
				test.Fatalf("guarded decode: state=%+v error=%v body=%x", decoded, err, guarded)
			}
			for offset := range guarded {
				guarded[offset] ^= 0xff
			}
			if decoded != fixture.state {
				test.Fatal("decoded record aliases input bytes")
			}
			encoded[0] ^= 0xff
			fresh, err := decoded.encode(context.Background(), bounds)
			if err != nil || !bytes.Equal(fresh, fixture.body[:]) || fixture.state.Version != 1 {
				test.Fatalf("reencode after independent buffer mutation: body=%x error=%v", fresh, err)
			}
			fresh[1] ^= 0xff
			if encoded[1] != fixture.body[1] {
				test.Fatal("encoder outputs alias one another")
			}
		})
	}
	test.Run("source_maximum_inherited_from_scalar", func(test *testing.T) {
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1}, bounds, true)
		oversized := bounds
		oversized.PayloadBytes++
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1}, oversized, false)
		platformMaximum := uint64(^uint(0) >> 1)
		if bounds.PayloadBytes < platformMaximum {
			scalar := legacyNativeScalarCheckpoint{Version: 1, Maximum: int(bounds.PayloadBytes)}
			if err := scalar.validate(MaxFinalizedDocBytes); err != nil {
				test.Fatalf("inherited scalar maximum rejected: %v", err)
			}
			scalar.Maximum++
			if err := scalar.validate(MaxFinalizedDocBytes); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("scalar accepted maximum plus one: %v", err)
			}
		}
	})
	test.Run("exact_bounds_without_graph_claims", func(test *testing.T) {
		zero := legacyNativeValueCheckpointBounds{}
		for _, kind := range []uint8{0, 1, 2, 3} {
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind}, zero, true)
		}
		for _, kind := range []uint8{4, 5} {
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind, Nil: true}, zero, true)
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind, Reference: 1}, zero, false)
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind, Reference: bounds.Nodes}, bounds, true)
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind, Reference: bounds.Nodes + 1}, bounds, false)
		}
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 3, Reference: bounds.PayloadBytes}, bounds, true)
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 3, Reference: bounds.PayloadBytes, Length: 1, Capacity: 1}, bounds, false)
		platformMaximum := uint64(^uint(0) >> 1)
		textLength := bounds.PayloadBytes
		if textLength > platformMaximum {
			textLength = platformMaximum
		}
		physicalMaximum := 2*bounds.PayloadBytes + 64
		if physicalMaximum > platformMaximum {
			physicalMaximum = platformMaximum
		}
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 3, Length: textLength, Capacity: physicalMaximum}, bounds, true)
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 3, Capacity: physicalMaximum + 1}, bounds, false)
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 4, Reference: 1, Length: bounds.Nodes, Capacity: bounds.Nodes}, bounds, true)
		legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 5, Reference: 1, Length: bounds.Nodes}, bounds, true)
	})
	test.Run("logical_slots_include_inline_leaves_not_only_containers", func(test *testing.T) {
		for _, kind := range []uint8{4, 5} {
			logicalSlots := legacyNativeValueCheckpointBounds{Nodes: 7}
			state := legacyNativeValueCheckpoint{Version: 1, Kind: kind, Reference: 1, Length: 6}
			if kind == 4 {
				state.Capacity = 6
			}
			legacyNativeValueWireOracleCheck(test, state, logicalSlots, true)
			logicalSlots.Nodes = 1
			legacyNativeValueWireOracleCheck(test, state, logicalSlots, false)
			logicalSlots.Nodes = 7
			state.Reference, state.Length, state.Capacity = 7, 0, 0
			legacyNativeValueWireOracleCheck(test, state, logicalSlots, true)
			logicalSlots.Nodes = 1
			legacyNativeValueWireOracleCheck(test, state, logicalSlots, false)
		}
	})
}

func TestLegacyNativeValueCheckpointWireHostile(test *testing.T) {
	bounds := legacyNativeValueWireOracleBounds()
	base := legacyNativeValueCheckpoint{Version: 1, Kind: 1, Bits: 1}
	test.Run("exact_length", func(test *testing.T) {
		legacyNativeValueWireOracleDecodeRejected(test, nil, bounds)
		golden := legacyNativeValueWireOracleBody(base)
		for length := 0; length <= 81; length++ {
			if length == 40 {
				continue
			}
			body := make([]byte, length)
			copy(body, golden[:])
			legacyNativeValueWireOracleDecodeRejected(test, body, bounds)
		}
		legacyNativeValueWireOracleDecodeRejected(test, append(golden[:], golden[:]...), bounds)
	})
	test.Run("every_reserved_octet_and_flag", func(test *testing.T) {
		for offset := 3; offset < 8; offset++ {
			for value := 1; value <= 255; value++ {
				body := legacyNativeValueWireOracleBody(base)
				body[offset] = byte(value)
				legacyNativeValueWireOracleDecodeRejected(test, body[:], bounds)
			}
		}
		for flag := 2; flag <= 255; flag++ {
			body := legacyNativeValueWireOracleBody(base)
			body[2] = byte(flag)
			legacyNativeValueWireOracleDecodeRejected(test, body[:], bounds)
		}
	})
	test.Run("every_unsupported_version_and_kind", func(test *testing.T) {
		for version := 0; version <= 255; version++ {
			if version != 1 {
				state := base
				state.Version = uint8(version)
				legacyNativeValueWireOracleCheck(test, state, bounds, false)
			}
		}
		for kind := 6; kind <= 255; kind++ {
			state := base
			state.Kind = uint8(kind)
			legacyNativeValueWireOracleCheck(test, state, bounds, false)
		}
	})
	test.Run("unused_fields_and_nil_canonicality", func(test *testing.T) {
		for _, fixture := range legacyNativeValueWireOracleFixtures() {
			for _, magnitude := range []uint64{1, 2, 1 << 32, 1 << 63, ^uint64(0)} {
				for _, field := range []string{"bits", "reference", "length", "capacity", "nil"} {
					state := fixture.state
					rejected := false
					switch field {
					case "bits":
						state.Bits = magnitude
						rejected = state.Kind == 0 || state.Kind == 1 && magnitude > 1 || state.Kind >= 3
					case "reference":
						state.Reference = magnitude
						rejected = state.Kind <= 2 || state.Nil
					case "length":
						state.Length = magnitude
						rejected = state.Kind <= 2 || state.Nil
					case "capacity":
						state.Capacity = magnitude
						rejected = state.Kind <= 2 || state.Kind == 5 || state.Nil
					case "nil":
						state.Nil = true
						rejected = state.Kind <= 3 || state.Reference != 0 || state.Length != 0 || state.Capacity != 0
					}
					if rejected {
						legacyNativeValueWireOracleCheck(test, state, bounds, false)
					}
				}
			}
		}
		for _, kind := range []uint8{4, 5} {
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind}, bounds, false)
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: kind, Nil: true}, bounds, true)
		}
	})
	test.Run("nonfinite_float_bits", func(test *testing.T) {
		for _, pattern := range []uint64{
			0x7ff0000000000000, 0xfff0000000000000,
			0x7ff0000000000001, 0xfff0000000000001,
			0x7ff8000000000000, 0xfff8000000000000,
			0x7fffffffffffffff, 0xffffffffffffffff,
		} {
			legacyNativeValueWireOracleCheck(test, legacyNativeValueCheckpoint{Version: 1, Kind: 2, Bits: pattern}, bounds, false)
		}
	})
	test.Run("global_bound_overflow", func(test *testing.T) {
		for _, fixture := range legacyNativeValueWireOracleFixtures() {
			for _, payload := range []uint64{bounds.PayloadBytes + 1, 1 << 63, ^uint64(0)} {
				overflow := bounds
				overflow.PayloadBytes = payload
				legacyNativeValueWireOracleCheck(test, fixture.state, overflow, false)
			}
			for _, nodes := range []uint64{bounds.Nodes + 1, 1 << 63, ^uint64(0)} {
				overflow := bounds
				overflow.Nodes = nodes
				legacyNativeValueWireOracleCheck(test, fixture.state, overflow, false)
			}
		}
	})
	test.Run("string_offset_length_capacity_overflow_matrix", func(test *testing.T) {
		local := legacyNativeValueCheckpointBounds{PayloadBytes: 17, Nodes: 7}
		platformMaximum := uint64(^uint(0) >> 1)
		physicalMaximum := 2*bounds.PayloadBytes + 64
		references := []uint64{0, 1, 16, 17, 18, 1 << 63, ^uint64(0)}
		lengths := []uint64{0, 1, 16, 17, 18, platformMaximum, platformMaximum + 1, 1 << 63, ^uint64(0)}
		capacities := []uint64{0, 1, 16, 17, 18, physicalMaximum, physicalMaximum + 1, platformMaximum, platformMaximum + 1, 1 << 63, ^uint64(0)}
		for _, reference := range references {
			for _, length := range lengths {
				end, carry := bits.Add64(reference, length, 0)
				for _, capacity := range capacities {
					accepted := carry == 0 && end <= local.PayloadBytes && length <= platformMaximum &&
						capacity >= length && capacity <= physicalMaximum && capacity <= platformMaximum
					state := legacyNativeValueCheckpoint{Version: 1, Kind: 3, Reference: reference, Length: length, Capacity: capacity}
					legacyNativeValueWireOracleCheck(test, state, local, accepted)
				}
			}
		}
	})
	test.Run("logical_slot_count_and_array_capacity_overflow_matrix", func(test *testing.T) {
		local := legacyNativeValueCheckpointBounds{Nodes: 7}
		platformMaximum := uint64(^uint(0) >> 1)
		for _, kind := range []uint8{4, 5} {
			for _, reference := range []uint64{0, 1, 7, 8, bounds.Nodes, bounds.Nodes + 1, 1 << 63, ^uint64(0)} {
				for _, length := range []uint64{0, 1, 7, 8, bounds.Nodes + 1, platformMaximum + 1, 1 << 63, ^uint64(0)} {
					for _, capacity := range []uint64{0, 1, 6, 7, 8, bounds.Nodes, bounds.Nodes + 1, platformMaximum + 1, 1 << 63, ^uint64(0)} {
						state := legacyNativeValueCheckpoint{Version: 1, Kind: kind, Reference: reference, Length: length, Capacity: capacity}
						accepted := reference >= 1 && reference <= local.Nodes && length <= local.Nodes && length <= platformMaximum
						if kind == 4 {
							accepted = accepted && capacity >= length && capacity <= uint64(MaxFinalizedDocBytes) && capacity <= platformMaximum
						} else {
							accepted = accepted && capacity == 0
						}
						legacyNativeValueWireOracleCheck(test, state, local, accepted)
						state.Nil = true
						legacyNativeValueWireOracleCheck(test, state, local, reference == 0 && length == 0 && capacity == 0)
					}
				}
			}
		}
	})
}

type legacyNativeValueWireOracleCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	after  int
}

func (current *legacyNativeValueWireOracleCancelContext) Err() error {
	current.calls++
	if current.calls >= current.after {
		current.cancel()
	}
	return current.Context.Err()
}

func TestLegacyNativeValueCheckpointWireCancellationAndInline(test *testing.T) {
	bounds := legacyNativeValueWireOracleBounds()
	for _, fixture := range legacyNativeValueWireOracleFixtures() {
		test.Run("inline/"+fixture.name, func(test *testing.T) {
			decoded, err := decodeLegacyNativeValueCheckpoint(context.Background(), fixture.body[:], bounds)
			if err != nil {
				test.Fatal(err)
			}
			value, ready, err := decoded.inline(context.Background(), bounds)
			if err != nil || ready != fixture.ready || !reflect.DeepEqual(value, fixture.inline) {
				test.Fatalf("inline value=%#v ready=%v error=%v; expected value=%#v ready=%v", value, ready, err, fixture.inline, fixture.ready)
			}
			if expected, floating := fixture.inline.(float64); floating {
				actual, correctType := value.(float64)
				if !correctType || math.Float64bits(actual) != math.Float64bits(expected) {
					test.Fatalf("inline changed finite float bits: value=%#v expected=%016x", value, math.Float64bits(expected))
				}
			}
		})
	}
	test.Run("cancellation_precedes_validation", func(test *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		expired, release := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer release()
		contexts := []struct {
			name    string
			current context.Context
			failure error
		}{
			{name: "canceled", current: canceled, failure: context.Canceled},
			{name: "deadline", current: expired, failure: context.DeadlineExceeded},
		}
		for _, entry := range contexts {
			test.Run(entry.name, func(test *testing.T) {
				fixtures := legacyNativeValueWireOracleFixtures()
				fixtures = append(fixtures, legacyNativeValueWireOracleFixture{state: legacyNativeValueCheckpoint{Version: 255, Kind: 255, Nil: true, Bits: ^uint64(0)}})
				for _, fixture := range fixtures {
					for _, currentBounds := range []legacyNativeValueCheckpointBounds{bounds, {PayloadBytes: ^uint64(0), Nodes: ^uint64(0)}} {
						body, err := fixture.state.encode(entry.current, currentBounds)
						if body != nil || !errors.Is(err, entry.failure) {
							test.Fatalf("encode cancellation: state=%+v body=%x error=%v", fixture.state, body, err)
						}
						value, ready, err := fixture.state.inline(entry.current, currentBounds)
						if value != nil || ready || !errors.Is(err, entry.failure) {
							test.Fatalf("inline cancellation: state=%+v value=%#v ready=%v error=%v", fixture.state, value, ready, err)
						}
						reserved := fixture.body
						reserved[7] = 1
						flags := fixture.body
						flags[2] = 255
						for _, body := range [][]byte{nil, fixture.body[:1], fixture.body[:], reserved[:], flags[:]} {
							before := append([]byte(nil), body...)
							decoded, err := decodeLegacyNativeValueCheckpoint(entry.current, body, currentBounds)
							if decoded != (legacyNativeValueCheckpoint{}) || !errors.Is(err, entry.failure) || !bytes.Equal(body, before) {
								test.Fatalf("decode cancellation: body=%x decoded=%+v error=%v", body, decoded, err)
							}
						}
					}
				}
			})
		}
	})
	test.Run("final_cancellation_discards_record_and_bytes", func(test *testing.T) {
		for _, fixture := range legacyNativeValueWireOracleFixtures() {
			for _, operation := range []string{"encode", "decode"} {
				test.Run(fmt.Sprintf("%s/%s", fixture.name, operation), func(test *testing.T) {
					created, cancel := context.WithCancel(context.Background())
					defer cancel()
					current := &legacyNativeValueWireOracleCancelContext{Context: created, cancel: cancel, after: 2}
					if operation == "encode" {
						body, err := fixture.state.encode(current, bounds)
						if body != nil || !errors.Is(err, context.Canceled) {
							test.Fatalf("final encode cancellation: body=%x error=%v", body, err)
						}
					} else {
						body := fixture.body
						decoded, err := decodeLegacyNativeValueCheckpoint(current, body[:], bounds)
						if decoded != (legacyNativeValueCheckpoint{}) || !errors.Is(err, context.Canceled) || body != fixture.body {
							test.Fatalf("final decode cancellation: state=%+v body=%x error=%v", decoded, body, err)
						}
					}
					if current.calls != 2 || !errors.Is(created.Err(), context.Canceled) {
						test.Fatalf("final cancellation was not reached: checks=%d error=%v", current.calls, created.Err())
					}
				})
			}
		}
	})
}
