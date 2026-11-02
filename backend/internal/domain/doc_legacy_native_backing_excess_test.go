package domain

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeBackingExcessMeasurement struct {
	capacity int
	length   int
	count    int
}

type legacyNativeBackingExcessObserver struct {
	scalar       *legacyNativeScalar
	field        int
	measurements map[string]legacyNativeBackingExcessMeasurement
}

func (observer *legacyNativeBackingExcessObserver) observe(test *testing.T, normalizer *LegacyNormalizationDecoder) {
	test.Helper()
	if normalizer.native == nil {
		if observer.scalar != nil {
			test.Fatal("scalar backing capacity was not captured before canonical handoff")
		}
		return
	}
	object := normalizer.native.object
	if observer.scalar != nil && object.scalar == nil {
		scalar := observer.scalar
		if scalar.text == nil || scalar.previous != nil || scalar.position != scalar.end || scalar.buffered != 0 {
			test.Fatal("native scalar disappeared before its builder was complete")
		}
		field := object.metadata.fields[observer.field]
		decoded := object.destination.Elem().Field(field.index)
		text := decoded
		if text.Kind() == reflect.Interface && !text.IsNil() {
			text = text.Elem()
		}
		if text.Kind() != reflect.String || text.String() != scalar.text.String() {
			test.Fatal("completed builder no longer backs its committed native field", field.name)
		}
		capacity, length := scalar.text.Cap(), scalar.text.Len()
		if capacity <= length || length <= LegacyCanonicalEncodingBytes {
			test.Fatal("giant scalar fixture did not expose actual spare builder capacity", field.name, capacity, length)
		}
		if decoded.Kind() == reflect.String {
			minimum, err := legacyNativeValueHeap(context.Background(), decoded)
			committed := object.heaps[observer.field]
			if err != nil || committed.err != nil || committed.bytes-minimum.bytes != 2*int64(capacity-length) {
				test.Fatal("committed native heap lost actual builder capacity before handoff", field.name, committed, minimum, capacity, length, err)
			}
		}
		prior := observer.measurements[field.name]
		observer.measurements[field.name] = legacyNativeBackingExcessMeasurement{capacity: capacity, length: length, count: prior.count + 1}
		observer.scalar = nil
	}
	if object.scalar != nil {
		if observer.scalar != nil && observer.scalar != object.scalar {
			test.Fatal("a later scalar replaced the builder before its capacity was captured")
		}
		observer.scalar, observer.field = object.scalar, object.field
	}
}

type legacyNativeBackingExcessFixture struct {
	name     string
	event    string
	want     CIREvent
	observed map[string]int
	retained []string
}

func legacyNativeBackingExcessCheck(test *testing.T, fixture legacyNativeBackingExcessFixture) {
	test.Helper()
	raw := legacyNativeEventDocument("2", fixture.event)
	normalizer, _ := legacyNativeScalarReviewStart(test, raw)
	handoff := &legacyCanonicalNativeHandoffObservation{normalizer: normalizer}
	observer := legacyNativeBackingExcessObserver{measurements: make(map[string]legacyNativeBackingExcessMeasurement)}
	want, err := canonicalEventJSONObject(fixture.want)
	if err != nil {
		test.Fatal("invalid expected canonical event", err)
	}
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	seen := false
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		observer.observe(test, normalizer)
		ctx, cancel := context.WithCancel(context.Background())
		handoff.Context = ctx
		step, err := normalizer.Step(handoff)
		cancel()
		if err != nil {
			test.Fatal("full normalizer failed before completing the backing handoff fixture", stepIndex, err)
		}
		observer.observe(test, normalizer)
		if len(step.Events) != 0 {
			if seen || len(step.Events) != 1 || step.Events[0].Encoding == nil || step.Events[0].Sequence != fixture.want.Seq {
				test.Fatal("unexpected pending canonical event batch", seen, len(step.Events))
			}
			seen = true
			encoding := step.Events[0].Encoding
			fields, ok := encoding.root.(map[string]any)
			if !ok || !reflect.DeepEqual(fields, want) {
				test.Fatal("native duplicate handling changed the emitted canonical event")
			}
			for field, count := range fixture.observed {
				if observer.measurements[field].count != count {
					test.Fatal("required native builder was not measured before handoff", field, observer.measurements[field].count, count)
				}
			}
			var excess int64
			for _, field := range fixture.retained {
				measurement, measured := observer.measurements[field]
				if !measured {
					test.Fatal("retained scalar has no observed capacity provenance", field)
				}
				text, ok := fields[field].(string)
				if !ok || len(text) != measurement.length {
					test.Fatal("retained scalar does not match its final measured builder length", field, len(text), measurement.length)
				}
				excess += 2 * int64(measurement.capacity-measurement.length)
			}
			handoff.check(test, encoding, excess)
		}
		if step.Complete {
			if !seen || step.Count != 1 {
				test.Fatal("full normalizer completed without exactly one measured handoff", seen, step.Count)
			}
			return
		}
	}
	test.Fatal("backing handoff fixture exhausted its bounded per-part progress allowance")
}

func TestLegacyNativeBackingExcessCanonicalHandoff(test *testing.T) {
	payload := strings.Repeat("x", 2*ConversationManifestChunkBytes+73)
	output := strings.Repeat("y", 2*ConversationManifestChunkBytes+91)
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	giantID := `"` + payload + `"` + padding
	giantOutput := `"` + output + `"` + padding
	for _, fixture := range []legacyNativeBackingExcessFixture{
		{
			name:     "giant-id",
			event:    `{"kind":"turn","seq":7,"id":` + giantID + `}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7, ID: payload},
			observed: map[string]int{"id": 1},
			retained: []string{"id"},
		},
		{
			name:     "giant-tool-result-output-with-id-provenance",
			event:    `{"kind":"tool_result","seq":7,"id":` + giantID + `,"output":` + giantOutput + `}`,
			want:     CIREvent{Kind: EventToolResult, Seq: 7, ID: payload, Output: output},
			observed: map[string]int{"id": 1},
			retained: []string{"id", "output"},
		},
		{
			name:     "giant-tool-result-output-only",
			event:    `{"kind":"tool_result","seq":7,"output":` + giantOutput + `}`,
			want:     CIREvent{Kind: EventToolResult, Seq: 7, Output: output},
			observed: map[string]int{"output": 1},
			retained: []string{"output"},
		},
		{
			name:     "two-committed-native-scalar-fields",
			event:    `{"kind":"tool_result","seq":7,"id":` + giantID + `,"call_id":` + giantID + `,"output":` + giantOutput + `}`,
			want:     CIREvent{Kind: EventToolResult, Seq: 7, ID: payload, CallID: payload, Output: output},
			observed: map[string]int{"id": 1, "call_id": 1},
			retained: []string{"id", "call_id", "output"},
		},
		{
			name:     "unemitted-native-scalar-does-not-transfer-excess",
			event:    `{"kind":"turn","seq":7,"status":` + giantID + `}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7},
			observed: map[string]int{"status": 1},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeBackingExcessCheck(test, fixture)
		})
	}
}

func TestLegacyNativeBackingExcessDuplicateFields(test *testing.T) {
	payload := strings.Repeat("x", 2*ConversationManifestChunkBytes+73)
	replacement := strings.Repeat("z", ConversationManifestChunkBytes+91)
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	giant := `"` + payload + `"` + padding
	shorterGiant := `"` + replacement + `"` + padding
	for _, fixture := range []legacyNativeBackingExcessFixture{
		{
			name:     "short-id-releases-old-excess",
			event:    `{"kind":"turn","seq":7,"id":` + giant + `,"ID":"short"}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7, ID: "short"},
			observed: map[string]int{"id": 1},
		},
		{
			name:     "empty-id-releases-old-excess-and-emitted-field",
			event:    `{"kind":"turn","seq":7,"id":` + giant + `,"ID":""}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7},
			observed: map[string]int{"id": 1},
		},
		{
			name:     "null-string-keeps-live-excess",
			event:    `{"kind":"turn","seq":7,"id":` + giant + `,"\u0069d":null}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7, ID: payload},
			observed: map[string]int{"id": 1},
			retained: []string{"id"},
		},
		{
			name:     "null-after-short-id-does-not-revive-old-excess",
			event:    `{"kind":"turn","seq":7,"id":` + giant + `,"ID":"short","\u0069d":null}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7, ID: "short"},
			observed: map[string]int{"id": 1},
		},
		{
			name:     "shorter-giant-replaces-rather-than-adds-excess",
			event:    `{"kind":"turn","seq":7,"id":` + giant + `,"ID":` + shorterGiant + `,"id":null}`,
			want:     CIREvent{Kind: EventTurn, Seq: 7, ID: replacement},
			observed: map[string]int{"id": 2},
			retained: []string{"id"},
		},
		{
			name:  "short-output-releases-any-old-output-excess",
			event: `{"kind":"tool_result","seq":7,"output":` + giant + `,"OUTPUT":"short"}`,
			want:  CIREvent{Kind: EventToolResult, Seq: 7, Output: "short"},
		},
		{
			name:  "null-interface-releases-any-old-output-excess",
			event: `{"kind":"tool_result","seq":7,"output":` + giant + `,"outp\u0075t":null}`,
			want:  CIREvent{Kind: EventToolResult, Seq: 7},
		},
		{
			name:     "null-output-does-not-drop-independent-id-excess",
			event:    `{"kind":"tool_result","seq":7,"id":` + giant + `,"output":` + giant + `,"OUTPUT":null}`,
			want:     CIREvent{Kind: EventToolResult, Seq: 7, ID: payload},
			observed: map[string]int{"id": 1},
			retained: []string{"id"},
		},
		{
			name:     "short-id-does-not-inherit-giant-output-provenance",
			event:    `{"kind":"tool_result","seq":7,"id":` + giant + `,"output":` + giant + `,"ID":"short"}`,
			want:     CIREvent{Kind: EventToolResult, Seq: 7, ID: "short", Output: payload},
			observed: map[string]int{"id": 1},
			retained: []string{"output"},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeBackingExcessCheck(test, fixture)
		})
	}
}
