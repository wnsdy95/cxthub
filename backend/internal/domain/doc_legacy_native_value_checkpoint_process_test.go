package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const legacyNativeValueCheckpointW76ChildMarker = "CXTHUB_W76_NATIVE_VALUE_RECORD_PORTABILITY_CHILD"
const legacyNativeValueCheckpointW76ChildDirectory = "CXTHUB_W76_NATIVE_VALUE_RECORD_PORTABILITY_DIRECTORY"

type legacyNativeValueCheckpointW76Fixture struct {
	name string
	raw  string
	bits uint64
}

type legacyNativeValueCheckpointW76Observation struct {
	State      legacyNativeValueCheckpoint `json:"state"`
	Inline     bool                        `json:"inline"`
	InlineType string                      `json:"inline_type"`
	InlineBits uint64                      `json:"inline_bits"`
	TextBytes  int                         `json:"text_bytes"`
	TextParts  int                         `json:"text_parts"`
	TextSHA    [32]byte                    `json:"text_sha"`
}

type legacyNativeValueCheckpointW76Report struct {
	ProcessID int                                         `json:"process_id"`
	Scope     string                                      `json:"scope"`
	Records   []legacyNativeValueCheckpointW76Observation `json:"records"`
}

type legacyNativeValueCheckpointW76CancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (current *legacyNativeValueCheckpointW76CancelContext) Err() error {
	current.checks++
	if current.checks == 2 {
		current.cancel()
	}
	return current.Context.Err()
}

func legacyNativeValueCheckpointW76Fixtures() []legacyNativeValueCheckpointW76Fixture {
	return []legacyNativeValueCheckpointW76Fixture{
		{name: "null", raw: "null"},
		{name: "false", raw: "false"},
		{name: "true", raw: "true"},
		{name: "positive-zero", raw: "0"},
		{name: "negative-zero", raw: "-0", bits: 0x8000000000000000},
		{name: "negative-decimal-zero", raw: "-0.0", bits: 0x8000000000000000},
		{name: "negative-one", raw: "-1", bits: 0xbff0000000000000},
		{name: "rounded-tenth", raw: "0.1", bits: 0x3fb999999999999a},
		{name: "positive-subnormal", raw: "5e-324", bits: 1},
		{name: "negative-subnormal", raw: "-5e-324", bits: 0x8000000000000001},
		{name: "minimum-normal", raw: "2.2250738585072014e-308", bits: 0x0010000000000000},
		{name: "maximum-finite", raw: "1.7976931348623157e308", bits: 0x7fefffffffffffff},
		{name: "rounded-integer", raw: "9007199254740993", bits: 0x4340000000000000},
		{name: "empty-text", raw: `""`},
		{name: "spare-capacity-text", raw: `"abc"`},
		{name: "decoded-text-not-raw", raw: "\"x\xff\\uD800\\n\\u0000\\uD83D\\uDE42\""},
		{name: "empty-array", raw: `[]`},
		{name: "array-spare-capacity", raw: `[null,true,-0]`},
		{name: "empty-object", raw: `{}`},
		{name: "object-accepted-duplicate", raw: `{"duplicate":1,"duplicate":null,"nested":[]}`},
		{name: "nested-container-reference-only", raw: `{"zero":-0,"array":[false,{"text":"value"}]}`},
	}
}

func legacyNativeValueCheckpointW76LongText() string {
	return `"` + strings.Repeat("x", 2*LegacyCanonicalEncodingBytes-1) + `\uD83D\uDE42\u0000line\n` +
		strings.Repeat("y", LegacyCanonicalEncodingBytes+13) + `"`
}

func legacyNativeValueCheckpointW76Accept(test *testing.T, raw string) (legacyNativeAnyValue, *legacySourceProofBuffer) {
	test.Helper()
	owner, err := newLegacyNativeAny(0, len(raw), 0, 64)
	if err != nil {
		test.Fatal("actual nativeAny constructor rejected fixture", err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	scalar := raw[0] != '[' && raw[0] != '{'
	if scalar {
		owner.tokenStart, owner.tokenEnd, owner.tokenFirst = 0, len(raw), raw[0]
		owner.applying = true
	}
	var frame legacyJSONValueScanner
	for stepIndex := 0; stepIndex < 128; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		var complete bool
		if scalar {
			complete, err = owner.decodeToken(ctx, input)
		} else {
			complete, err = owner.step(ctx, input, &frame)
		}
		cancel()
		if err != nil || input.failure != nil || owner.semanticError != nil {
			test.Fatal("actual accepted-value decoding failed", stepIndex, err, input.failure, owner.semanticError)
		}
		if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields < 0 || input.fields > 64 || input.values < 0 || input.values > legacyNativeAnyValueActions {
			test.Fatal("actual accepted-value decoding escaped its shared quantum", input)
		}
		for pageIndex := range input.pages {
			clear(input.pages[pageIndex])
		}
		if !complete {
			continue
		}
		if scalar {
			if !owner.pending || owner.applying || owner.scalar != nil || owner.number != nil {
				test.Fatal("decodeToken did not leave its accepted scalar in node")
			}
			return owner.node, buffer
		}
		if !owner.finished || owner.position != len(raw) || owner.frame != nil || owner.depth != 0 || owner.pending {
			test.Fatal("actual container step did not leave a finished result")
		}
		return owner.result, buffer
	}
	test.Fatal("actual accepted-value fixture exhausted its finite step schedule")
	return legacyNativeAnyValue{}, nil
}

func legacyNativeValueCheckpointW76Capture(test *testing.T, value legacyNativeAnyValue, reference uint64, bounds legacyNativeValueCheckpointBounds) *legacyNativeValueSnapshot {
	test.Helper()
	snapshot, err := value.checkpoint(context.Background(), reference, bounds)
	if err != nil || snapshot == nil {
		test.Fatal("actual accepted value could not be captured", err)
	}
	expected := legacyNativeValueCheckpoint{Version: 1, Reference: reference}
	switch typed := value.value.(type) {
	case nil:
		expected.Kind = legacyNativeCheckpointNull
	case bool:
		expected.Kind = legacyNativeCheckpointBool
		if typed {
			expected.Bits = 1
		}
	case float64:
		expected.Kind, expected.Bits = legacyNativeCheckpointFloat64, math.Float64bits(typed)
	case string:
		expected.Kind, expected.Length, expected.Capacity = legacyNativeCheckpointString, uint64(len(typed)), uint64(value.capacity)
		if snapshot.text != typed || len(typed) > 0 && unsafe.StringData(snapshot.text) != unsafe.StringData(typed) {
			test.Fatal("snapshot cloned or changed the accepted text instead of borrowing it")
		}
	case []any:
		expected.Kind, expected.Nil = legacyNativeCheckpointArray, typed == nil
		expected.Length, expected.Capacity = uint64(len(typed)), uint64(cap(typed))
	case map[string]any:
		expected.Kind, expected.Nil, expected.Length = legacyNativeCheckpointObject, typed == nil, uint64(len(typed))
	default:
		test.Fatal("capture fixture is not a supported native value")
	}
	if snapshot.state != expected {
		test.Fatal("capture lost exact accepted type, bits, nilness, reference, length, or physical capacity", snapshot.state, expected)
	}
	expectedRetained := int64(64) + 2*int64(unsafe.Sizeof(*snapshot))
	if expected.Kind == legacyNativeCheckpointString && value.capacity > 0 {
		expectedRetained += 64 + 2*int64(value.capacity)
	}
	retained, err := snapshot.retainedBytes()
	if err != nil || retained != expectedRetained {
		test.Fatal("snapshot retention omitted capacity or retained the native container owner", retained, expectedRetained, err)
	}
	return snapshot
}

func TestLegacyNativeValueCheckpointW76AcceptedCapture(test *testing.T) {
	fixtures := legacyNativeValueCheckpointW76Fixtures()
	for fixtureIndex := range fixtures {
		fixture := fixtures[fixtureIndex]
		test.Run(fixture.name, func(test *testing.T) {
			value, buffer := legacyNativeValueCheckpointW76Accept(test, fixture.raw)
			var expected any
			if err := json.Unmarshal([]byte(fixture.raw), &expected); err != nil || !reflect.DeepEqual(value.value, expected) {
				test.Fatal("actual native value disagrees with accepted encoding/json value", value.value, expected, err)
			}
			if number, ok := value.value.(float64); ok && math.Float64bits(number) != fixture.bits {
				test.Fatalf("actual float bits=%016x, want=%016x", math.Float64bits(number), fixture.bits)
			}
			bounds := legacyNativeValueCheckpointBounds{Nodes: 64}
			var reference uint64
			switch typed := value.value.(type) {
			case string:
				reference, bounds.PayloadBytes = 11, uint64(11+len(typed)+7)
				if fixture.name == "spare-capacity-text" && value.capacity <= len(typed) {
					test.Fatal("text fixture did not exercise actual spare builder capacity")
				}
			case []any:
				reference = uint64(fixtureIndex + 1)
				if typed == nil || fixture.name == "array-spare-capacity" && cap(typed) != 4 {
					test.Fatal("actual JSON array lost nonnil empty or spare-capacity ownership", typed, cap(typed))
				}
			case map[string]any:
				reference = uint64(fixtureIndex + 1)
				if typed == nil {
					test.Fatal("actual JSON object became a typed nil map")
				}
			}
			reads := len(buffer.reads)
			snapshot := legacyNativeValueCheckpointW76Capture(test, value, reference, bounds)
			defer snapshot.close()
			body, err := snapshot.state.encode(context.Background(), bounds)
			if err != nil || len(body) != 40 {
				test.Fatal("accepted value did not encode as a 40-byte record", len(body), err)
			}
			decoded, err := decodeLegacyNativeValueCheckpoint(context.Background(), body, bounds)
			if err != nil || decoded != snapshot.state || len(buffer.reads) != reads {
				test.Fatal("record capture changed metadata or reread original RAW", decoded, err)
			}
			if snapshot.state.Kind != legacyNativeCheckpointString {
				if part, err := snapshot.read(context.Background(), 0, 0); part != nil || !errors.Is(err, ErrIntegrity) {
					test.Fatal("nontext snapshot exposed a pretend container payload", part, err)
				}
			}
		})
	}
	test.Run("typed-nil-distinct-from-decoded-empty", func(test *testing.T) {
		for _, typedNil := range []any{[]any(nil), map[string]any(nil)} {
			value := legacyNativeAnyValue{value: typedNil}
			snapshot := legacyNativeValueCheckpointW76Capture(test, value, 0, legacyNativeValueCheckpointBounds{})
			if !snapshot.state.Nil || snapshot.state.Reference != 0 || snapshot.state.Length != 0 || snapshot.state.Capacity != 0 {
				test.Fatal("typed nil record was confused with a decoded nonnil empty container", snapshot.state)
			}
			restored, ok, err := snapshot.state.inline(context.Background(), legacyNativeValueCheckpointBounds{})
			if err != nil || !ok || restored == nil || !reflect.DeepEqual(restored, typedNil) {
				test.Fatal("typed nil inline value lost its dynamic type", restored, ok, err)
			}
			snapshot.close()
		}
	})
	test.Run("actual-value-metadata-rejection", func(test *testing.T) {
		accepted, buffer := legacyNativeValueCheckpointW76Accept(test, `"abc"`)
		reads := len(buffer.reads)
		failure := errors.New("W76 accepted-value heap failure")
		fixtures := []struct {
			name  string
			value legacyNativeAnyValue
			err   error
		}{
			{name: "negative-heap", value: legacyNativeAnyValue{heap: docRangeHeaderHeapEstimate{bytes: -1}}, err: ErrIntegrity},
			{name: "negative-excess", value: legacyNativeAnyValue{excess: -1}, err: ErrIntegrity},
			{name: "excess-over-heap", value: legacyNativeAnyValue{excess: 1}, err: ErrIntegrity},
			{name: "negative-capacity", value: legacyNativeAnyValue{capacity: -1}, err: ErrIntegrity},
			{name: "nontext-capacity", value: legacyNativeAnyValue{value: true, capacity: 1}, err: ErrIntegrity},
			{name: "unsupported-native-type", value: legacyNativeAnyValue{value: int64(1)}, err: ErrIntegrity},
			{name: "heap-error", value: legacyNativeAnyValue{heap: docRangeHeaderHeapEstimate{err: failure}}, err: failure},
		}
		shortCapacity := accepted
		shortCapacity.capacity = 2
		fixtures = append(fixtures, struct {
			name  string
			value legacyNativeAnyValue
			err   error
		}{name: "accepted-text-capacity-below-length", value: shortCapacity, err: ErrIntegrity})
		for fixtureIndex := range fixtures {
			fixture := fixtures[fixtureIndex]
			test.Run(fixture.name, func(test *testing.T) {
				snapshot, err := fixture.value.checkpoint(context.Background(), 0, legacyNativeValueCheckpointBounds{PayloadBytes: 3})
				if snapshot != nil || !errors.Is(err, fixture.err) {
					test.Fatal("invalid accepted-value metadata produced a snapshot", snapshot, err)
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if snapshot, err := accepted.checkpoint(ctx, 0, legacyNativeValueCheckpointBounds{PayloadBytes: 3}); snapshot != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("pre-canceled accepted-value capture succeeded", snapshot, err)
		}
		base, stop := context.WithCancel(context.Background())
		defer stop()
		current := &legacyNativeValueCheckpointW76CancelContext{Context: base, cancel: stop}
		if snapshot, err := accepted.checkpoint(current, 0, legacyNativeValueCheckpointBounds{PayloadBytes: 3}); snapshot != nil || !errors.Is(err, context.Canceled) || current.checks != 2 {
			test.Fatal("capture ignored cancellation at its final checkpoint", snapshot, err, current.checks)
		}
		if len(buffer.reads) != reads {
			test.Fatal("metadata checks reread original RAW")
		}
	})
	test.Log("Actual scalar node and container result capture only. Typed nils are explicit typed-value fixtures, not JSON empty-container decode results. No full native graph, parser, or worker checkpoint is claimed.")
}

func TestLegacyNativeValueCheckpointW76TextSnapshotLifecycle(test *testing.T) {
	for _, raw := range []string{`""`, `"abc"`, "\"x\xff\\uD800\\n\\u0000\\uD83D\\uDE42\"", legacyNativeValueCheckpointW76LongText()} {
		test.Run(fmt.Sprintf("raw-bytes-%d", len(raw)), func(test *testing.T) {
			value, buffer := legacyNativeValueCheckpointW76Accept(test, raw)
			text, ok := value.value.(string)
			if !ok {
				test.Fatal("actual decodeToken did not accept text")
			}
			var expected string
			if err := json.Unmarshal([]byte(raw), &expected); err != nil || text != expected {
				test.Fatal("accepted native text differs from decoded text", err)
			}
			bounds := legacyNativeValueCheckpointBounds{PayloadBytes: uint64(11 + len(text) + 7)}
			reads := len(buffer.reads)
			beforeHeap, beforeExcess, beforeCapacity := value.heap, value.excess, value.capacity
			snapshot := legacyNativeValueCheckpointW76Capture(test, value, 11, bounds)
			defer snapshot.close()
			retained, err := snapshot.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			clear(buffer.body)
			for offset := 0; offset < len(text); offset += LegacyCanonicalEncodingBytes {
				length := min(LegacyCanonicalEncodingBytes, len(text)-offset)
				part, err := snapshot.read(context.Background(), offset, length)
				if err != nil || len(part) != length || len(part) > 64<<10 || string(part) != expected[offset:offset+length] ||
					unsafe.SliceData(part) == unsafe.StringData(snapshot.text[offset:]) {
					test.Fatal("snapshot did not return a bounded, copied accepted-text slice", offset, len(part), err)
				}
				part[0] ^= 0xff
				fresh, err := snapshot.read(context.Background(), offset, length)
				if err != nil || string(fresh) != expected[offset:offset+length] || unsafe.SliceData(fresh) == unsafe.SliceData(part) {
					test.Fatal("snapshot read aliased a prior caller-owned read or native text", offset, err)
				}
			}
			maximumInt := int(^uint(0) >> 1)
			for _, invalid := range [][2]int{{-1, 0}, {0, -1}, {len(text) + 1, 0}, {len(text), 1}, {0, (64 << 10) + 1}, {maximumInt, 1}, {1, maximumInt}} {
				part, err := snapshot.read(context.Background(), invalid[0], invalid[1])
				if part != nil || !errors.Is(err, ErrIntegrity) {
					test.Fatal("snapshot admitted an invalid, overflowing, or over-64KiB read", invalid, part, err)
				}
			}
			if part, err := snapshot.read(context.Background(), len(text), 0); err != nil || len(part) != 0 {
				test.Fatal("snapshot rejected the exact empty EOF range", part, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if part, err := snapshot.read(ctx, 0, min(1, len(text))); part != nil || !errors.Is(err, context.Canceled) {
				test.Fatal("snapshot ignored pre-read cancellation", part, err)
			}
			base, stop := context.WithCancel(context.Background())
			defer stop()
			current := &legacyNativeValueCheckpointW76CancelContext{Context: base, cancel: stop}
			if part, err := snapshot.read(current, 0, min(1, len(text))); part != nil || !errors.Is(err, context.Canceled) || current.checks != 2 {
				test.Fatal("snapshot returned copied bytes after final cancellation", part, err, current.checks)
			}
			corrupt := *snapshot
			corrupt.state.Length++
			if part, err := corrupt.read(context.Background(), 0, 0); part != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("snapshot accepted mismatched text length metadata", part, err)
			}
			corrupt = *snapshot
			corrupt.state.Capacity = uint64(maximumInt) + 1
			if retained, err := corrupt.retainedBytes(); retained != 0 || !errors.Is(err, ErrIntegrity) {
				test.Fatal("snapshot retention accepted an unrepresentable capacity", retained, err)
			}
			if len(text) > 0 {
				corrupt.state.Capacity = uint64(len(text) - 1)
				if retained, err := corrupt.retainedBytes(); retained != 0 || !errors.Is(err, ErrIntegrity) {
					test.Fatal("snapshot retention accepted capacity below accepted length", retained, err)
				}
			}
			afterRetained, err := snapshot.retainedBytes()
			if err != nil || retained != afterRetained || len(buffer.reads) != reads || value.value.(string) != expected ||
				value.heap != beforeHeap || value.excess != beforeExcess || value.capacity != beforeCapacity {
				test.Fatal("snapshot reads mutated native metadata, retained copied reads, or reread RAW", retained, afterRetained, err)
			}
			snapshot.close()
			snapshot.close()
			if !snapshot.closed || snapshot.text != "" || snapshot.state != (legacyNativeValueCheckpoint{}) {
				test.Fatal("snapshot close retained borrowed text or record fields")
			}
			if retained, err := snapshot.retainedBytes(); retained != 0 || err != nil {
				test.Fatal("closed snapshot retained bytes", retained, err)
			}
			if part, err := snapshot.read(context.Background(), 0, 0); part != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("closed snapshot remained readable", part, err)
			}
		})
	}
	var absent *legacyNativeValueSnapshot
	absent.close()
	if retained, err := absent.retainedBytes(); retained != 0 || err != nil {
		test.Fatal("nil snapshot retained bytes", retained, err)
	}
	if part, err := absent.read(context.Background(), 0, 0); part != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil snapshot remained readable", part, err)
	}
	test.Log("Zero-copy snapshot capture and separately copied reads up to 64KiB; retainedBytes is the snapshot/text-capacity estimator, not RSS or a complete native-owner heap bound.")
}

func legacyNativeValueCheckpointW76InlineObservation(test *testing.T, state legacyNativeValueCheckpoint, bounds legacyNativeValueCheckpointBounds) legacyNativeValueCheckpointW76Observation {
	test.Helper()
	value, ok, err := state.inline(context.Background(), bounds)
	if err != nil {
		test.Fatal("portable record inline operation failed", err)
	}
	observation := legacyNativeValueCheckpointW76Observation{State: state, Inline: ok}
	switch state.Kind {
	case legacyNativeCheckpointNull:
		if !ok || value != nil {
			test.Fatal("portable null record did not inline as nil", value, ok)
		}
		observation.InlineType = "nil"
	case legacyNativeCheckpointBool:
		boolean, typed := value.(bool)
		if !ok || !typed || boolean != (state.Bits == 1) {
			test.Fatal("portable boolean changed its actual inline value", value, ok)
		}
		observation.InlineType = "bool"
		if boolean {
			observation.InlineBits = 1
		}
	case legacyNativeCheckpointFloat64:
		number, typed := value.(float64)
		if !ok || !typed || math.Float64bits(number) != state.Bits {
			test.Fatal("portable float lost its exact finite bits", value, ok)
		}
		observation.InlineType, observation.InlineBits = "float64", math.Float64bits(number)
	case legacyNativeCheckpointArray:
		if state.Nil {
			array, typed := value.([]any)
			if !ok || !typed || array != nil || value == nil {
				test.Fatal("portable nil array lost its dynamic type", value, ok)
			}
			observation.InlineType = "[]any(nil)"
		} else if ok || value != nil {
			test.Fatal("logical array reference pretended to restore an array graph", value, ok)
		}
	case legacyNativeCheckpointObject:
		if state.Nil {
			object, typed := value.(map[string]any)
			if !ok || !typed || object != nil || value == nil {
				test.Fatal("portable nil object lost its dynamic type", value, ok)
			}
			observation.InlineType = "map[string]any(nil)"
		} else if ok || value != nil {
			test.Fatal("logical object reference pretended to restore a map graph", value, ok)
		}
	case legacyNativeCheckpointString:
		if ok || value != nil {
			test.Fatal("text reference pretended to inline a restored native string", value, ok)
		}
	default:
		test.Fatal("portable record contains an unknown kind", state.Kind)
	}
	return observation
}

func TestLegacyNativeValueCheckpointW76RecordPortability(test *testing.T) {
	directory := test.TempDir()
	fixtures := append(legacyNativeValueCheckpointW76Fixtures(), legacyNativeValueCheckpointW76Fixture{name: "streamed-text", raw: legacyNativeValueCheckpointW76LongText()})
	var values []legacyNativeAnyValue
	for fixtureIndex := range fixtures {
		value, buffer := legacyNativeValueCheckpointW76Accept(test, fixtures[fixtureIndex].raw)
		values = append(values, value)
		clear(buffer.body)
	}
	values = append(values, legacyNativeAnyValue{value: []any(nil)}, legacyNativeAnyValue{value: map[string]any(nil)})
	var states []legacyNativeValueCheckpoint
	var expected []legacyNativeValueCheckpointW76Observation
	expectedText := make(map[int][]byte)
	payload := []byte("W76-PAYLOAD-PREFIX")
	for valueIndex := range values {
		value := values[valueIndex]
		bounds := legacyNativeValueCheckpointBounds{Nodes: 64, PayloadBytes: uint64(len(payload))}
		var reference uint64
		switch typed := value.value.(type) {
		case string:
			reference = uint64(len(payload))
			bounds.PayloadBytes += uint64(len(typed))
		case []any:
			if typed != nil {
				reference = uint64(valueIndex + 1)
			}
		case map[string]any:
			if typed != nil {
				reference = uint64(valueIndex + 1)
			}
		}
		snapshot := legacyNativeValueCheckpointW76Capture(test, value, reference, bounds)
		observation := legacyNativeValueCheckpointW76Observation{State: snapshot.state}
		switch typed := value.value.(type) {
		case nil:
			observation.Inline, observation.InlineType = true, "nil"
		case bool:
			observation.Inline, observation.InlineType = true, "bool"
			if typed {
				observation.InlineBits = 1
			}
		case float64:
			observation.Inline, observation.InlineType, observation.InlineBits = true, "float64", math.Float64bits(typed)
		case string:
			expectedText[valueIndex] = []byte(typed)
			observation.TextBytes, observation.TextSHA = len(typed), sha256.Sum256([]byte(typed))
			observation.TextParts = (len(typed) + (64 << 10) - 1) / (64 << 10)
			for offset := 0; offset < len(typed); offset += LegacyCanonicalEncodingBytes {
				part, err := snapshot.read(context.Background(), offset, min(LegacyCanonicalEncodingBytes, len(typed)-offset))
				if err != nil || len(part) > 64<<10 {
					test.Fatal("persisting accepted text exceeded the copied-read bound", err)
				}
				payload = append(payload, part...)
			}
			payload = append(payload, []byte("W76-PAYLOAD-SEPARATOR")...)
		case []any:
			if typed == nil {
				observation.Inline, observation.InlineType = true, "[]any(nil)"
			}
		case map[string]any:
			if typed == nil {
				observation.Inline, observation.InlineType = true, "map[string]any(nil)"
			}
		}
		states, expected = append(states, snapshot.state), append(expected, observation)
		snapshot.close()
	}
	allBounds := make([]legacyNativeValueCheckpointBounds, len(states))
	for recordIndex := range states {
		allBounds[recordIndex] = legacyNativeValueCheckpointBounds{PayloadBytes: uint64(len(payload)), Nodes: 64}
		body, err := states[recordIndex].encode(context.Background(), allBounds[recordIndex])
		if err != nil || len(body) != 40 {
			test.Fatal("portable record must have exactly 40 bytes", len(body), err)
		}
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("record-%02d.bin", recordIndex)), body, 0600); err != nil {
			test.Fatal(err)
		}
	}
	body, err := json.Marshal(allBounds)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "bounds.json"), body, 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "payload.bin"), payload, 0600); err != nil {
		test.Fatal(err)
	}
	clear(values)
	clear(fixtures)
	clear(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNativeValueCheckpointW76RecordPortabilityChild$", "-test.count=1", "-test.timeout=20s")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, legacyNativeValueCheckpointW76ChildMarker+"=") && !strings.HasPrefix(variable, legacyNativeValueCheckpointW76ChildDirectory+"=") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, legacyNativeValueCheckpointW76ChildMarker+"=1", legacyNativeValueCheckpointW76ChildDirectory+"="+directory)
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("W76 record-portability child failed: %v\n%s", err, output)
	}
	body, err = os.ReadFile(filepath.Join(directory, "report.json"))
	if err != nil {
		test.Fatal(err)
	}
	var report legacyNativeValueCheckpointW76Report
	if err := json.Unmarshal(body, &report); err != nil {
		test.Fatal(err)
	}
	if report.ProcessID <= 0 || report.ProcessID == os.Getpid() || report.Scope != "40-byte-record-portability-only" || !reflect.DeepEqual(report.Records, expected) {
		test.Fatal("new process changed actual scalar bits, typed nils, metadata, unsupported-reference status, or streamed text", report, expected)
	}
	for recordIndex, expectedBytes := range expectedText {
		actual, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf("text-%02d.bin", recordIndex)))
		if err != nil || !bytes.Equal(actual, expectedBytes) || sha256.Sum256(actual) != expected[recordIndex].TextSHA {
			test.Fatal("child streamed different accepted-text bytes or hash", recordIndex, err)
		}
	}
	test.Logf("record portability only: child pid=%d, records=%d, streamed text records=%d; child input is saved 40-byte records, payload, and bounds only", report.ProcessID, len(report.Records), len(expectedText))
	test.Log("Scalars and explicit typed nils inline; text and nonnil containers, including empty ones, remain ok=false. Payload text bytes/hash are transferred, not restored as a native graph. No original RAW, native owner, logical-reference binding, parser state, or worker checkpoint is available to the child.")
}

func TestLegacyNativeValueCheckpointW76RecordPortabilityChild(test *testing.T) {
	if os.Getenv(legacyNativeValueCheckpointW76ChildMarker) != "1" {
		test.Skip("W76 subprocess child helper, explicitly excluded from the three parent groups")
	}
	directory := os.Getenv(legacyNativeValueCheckpointW76ChildDirectory)
	body, err := os.ReadFile(filepath.Join(directory, "bounds.json"))
	if err != nil {
		test.Fatal(err)
	}
	var allBounds []legacyNativeValueCheckpointBounds
	if err := json.Unmarshal(body, &allBounds); err != nil || len(allBounds) == 0 || len(allBounds) > 64 {
		test.Fatal("child bounds exceed the finite record fixture", err)
	}
	payload, err := os.Open(filepath.Join(directory, "payload.bin"))
	if err != nil {
		test.Fatal(err)
	}
	defer payload.Close()
	info, err := payload.Stat()
	if err != nil || info.Size() < 0 || info.Size() > 1<<20 {
		test.Fatal("child payload exceeds the finite record-portability fixture", err)
	}
	report := legacyNativeValueCheckpointW76Report{ProcessID: os.Getpid(), Scope: "40-byte-record-portability-only"}
	for recordIndex := range allBounds {
		bounds := allBounds[recordIndex]
		if bounds.PayloadBytes != uint64(info.Size()) || bounds.Nodes > 64 {
			test.Fatal("child payload or logical-slot bound differs from the saved bounds", bounds)
		}
		body, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf("record-%02d.bin", recordIndex)))
		if err != nil || len(body) != 40 {
			test.Fatal("child did not receive an exact 40-byte saved record", recordIndex, len(body), err)
		}
		state, err := decodeLegacyNativeValueCheckpoint(context.Background(), body, bounds)
		if err != nil {
			test.Fatal("child could not decode the saved record", recordIndex, err)
		}
		observation := legacyNativeValueCheckpointW76InlineObservation(test, state, bounds)
		if state.Kind == legacyNativeCheckpointString {
			output, err := os.Create(filepath.Join(directory, fmt.Sprintf("text-%02d.bin", recordIndex)))
			if err != nil {
				test.Fatal(err)
			}
			reader := io.NewSectionReader(payload, int64(state.Reference), int64(state.Length))
			hasher := sha256.New()
			chunk := make([]byte, 64<<10)
			for observation.TextBytes < int(state.Length) {
				length := min(len(chunk), int(state.Length)-observation.TextBytes)
				count, err := io.ReadFull(reader, chunk[:length])
				if err != nil || count != length || count > 64<<10 {
					test.Fatal("child text read escaped the saved payload range or 64KiB bound", recordIndex, count, err)
				}
				written, err := output.Write(chunk[:count])
				if err != nil || written != count {
					test.Fatal("child could not stream accepted-text bytes", written, err)
				}
				hashed, err := hasher.Write(chunk[:count])
				if err != nil || hashed != count {
					test.Fatal("child could not hash streamed accepted-text bytes", hashed, err)
				}
				observation.TextBytes += count
				observation.TextParts++
			}
			if err := output.Close(); err != nil {
				test.Fatal(err)
			}
			if count, err := reader.Read(chunk[:1]); count != 0 || !errors.Is(err, io.EOF) {
				test.Fatal("child selected text range consumed adjacent payload bytes", count, err)
			}
			copy(observation.TextSHA[:], hasher.Sum(nil))
		}
		report.Records = append(report.Records, observation)
	}
	body, err = json.Marshal(report)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.json"), body, 0600); err != nil {
		test.Fatal(err)
	}
}
