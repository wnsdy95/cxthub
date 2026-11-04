package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeAnyOwnershipMeasurement struct {
	field    string
	text     string
	capacity int
	length   int
	key      bool
	depth    int
}

type legacyNativeAnyOwnershipActive struct {
	scalar *legacyNativeScalar
	key    bool
	depth  int
}

type legacyNativeAnyOwnershipObserver struct {
	helpers      map[*legacyNativeAny]string
	active       map[*legacyNativeAny]legacyNativeAnyOwnershipActive
	measurements []legacyNativeAnyOwnershipMeasurement
}

func newLegacyNativeAnyOwnershipObserver() *legacyNativeAnyOwnershipObserver {
	return &legacyNativeAnyOwnershipObserver{
		helpers: make(map[*legacyNativeAny]string),
		active:  make(map[*legacyNativeAny]legacyNativeAnyOwnershipActive),
	}
}

func (observer *legacyNativeAnyOwnershipObserver) observe(test *testing.T, native *legacyNativeObject) {
	test.Helper()
	if native != nil {
		for index, helper := range native.containers {
			if helper != nil {
				observer.helpers[helper] = native.metadata.fields[index].name
			}
		}
	}
	for helper, field := range observer.helpers {
		active, tracked := observer.active[helper]
		if tracked && helper.scalar != active.scalar {
			scalar := active.scalar
			if scalar.text == nil || scalar.previous != nil || scalar.position != scalar.end || scalar.buffered != 0 {
				test.Fatal("generic builder disappeared before its actual allocation could be measured", field, active.key)
			}
			capacity, length := scalar.text.Cap(), scalar.text.Len()
			if length <= LegacyCanonicalEncodingBytes || capacity <= length {
				test.Fatal("generic fixture did not expose actual spare builder capacity", field, active.key, capacity, length)
			}
			observer.measurements = append(observer.measurements, legacyNativeAnyOwnershipMeasurement{
				field: field, text: scalar.text.String(), capacity: capacity, length: length, key: active.key, depth: active.depth,
			})
			delete(observer.active, helper)
		}
		if helper.scalar != nil {
			depth := 0
			for current := helper.frame; current != nil; current = current.parent {
				if (current.object == nil) == (current.array == nil) || current.parent != nil && current.parent.object != nil && current.parent.key == "" {
					test.Fatal("observed generic scalar lost its linked owning frame", field)
				}
				depth++
			}
			if depth == 0 || helper.keyToken && helper.frame.object == nil {
				test.Fatal("generic key/value builder has no actual owning container", field, helper.keyToken)
			}
			observer.active[helper] = legacyNativeAnyOwnershipActive{scalar: helper.scalar, key: helper.keyToken, depth: depth}
		}
	}
}

func (observer *legacyNativeAnyOwnershipObserver) excess(test *testing.T, field, text string, key bool) int64 {
	test.Helper()
	capacity, length := -1, -1
	for _, measurement := range observer.measurements {
		if measurement.field != field || measurement.key != key || measurement.text != text {
			continue
		}
		if capacity >= 0 && (measurement.capacity != capacity || measurement.length != length) {
			test.Fatal("equal-key/value fixture no longer has equal actual builder allocations", field, key)
		}
		capacity, length = max(capacity, measurement.capacity), measurement.length
	}
	if capacity < 0 || length != len(text) {
		test.Fatal("emitted generic string has no actual key/value builder provenance", field, key, len(text))
	}
	return 2 * int64(capacity-length)
}

func (observer *legacyNativeAnyOwnershipObserver) valueExcess(test *testing.T, field string, value any) int64 {
	test.Helper()
	var excess int64
	switch decoded := value.(type) {
	case string:
		return observer.excess(test, field, decoded, false)
	case map[string]any:
		for key, member := range decoded {
			excess += observer.excess(test, field, key, true)
			excess += observer.valueExcess(test, field, member)
		}
	case []any:
		for _, member := range decoded {
			excess += observer.valueExcess(test, field, member)
		}
	}
	return excess
}

type legacyNativeAnyOwnershipFixture struct {
	name     string
	event    string
	observed map[string][2]int
	depth    int
}

func legacyNativeAnyOwnershipCheck(test *testing.T, fixture legacyNativeAnyOwnershipFixture) {
	test.Helper()
	raw := legacyNativeEventDocument("2", fixture.event)
	wantDocument, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal("installed native JSON oracle rejected the generic ownership fixture", err)
	}
	var expected CIREvent
	if err := json.Unmarshal([]byte(fixture.event), &expected); err != nil {
		test.Fatal("installed encoding/json rejected the generic event", err)
	}
	wantFields, err := canonicalEventJSONObject(expected)
	if err != nil {
		test.Fatal("generic native event cannot be projected canonically", err)
	}
	normalizer, source := legacyNativeScalarReviewStart(test, raw)
	handoff := &legacyCanonicalNativeHandoffObservation{Context: context.Background(), normalizer: normalizer}
	observer := newLegacyNativeAnyOwnershipObserver()
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	var eventBody []byte
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		if normalizer.nativeCache != nil {
			observer.observe(test, normalizer.nativeCache.object)
		}
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, source, handoff.withContext)
		if err != nil {
			test.Fatal("generic native decode failed before canonical handoff", stepIndex, err)
		}
		if normalizer.nativeCache != nil {
			observer.observe(test, normalizer.nativeCache.object)
		}
		if len(step.Events) != 0 {
			if eventBody != nil || len(step.Events) != 1 || step.Events[0].Encoding == nil || step.Events[0].Sequence != expected.Seq || len(observer.active) != 0 {
				test.Fatal("unexpected generic canonical handoff or unmeasured builder", len(step.Events), len(observer.active))
			}
			encoding := step.Events[0].Encoding
			fields, ok := encoding.root.(map[string]any)
			if !ok || !reflect.DeepEqual(fields, wantFields) {
				test.Fatal("generic merge/replacement or exact float64 leaves differ from encoding/json")
			}
			counts := make(map[string][2]int)
			depth := 0
			for _, measurement := range observer.measurements {
				count := counts[measurement.field]
				if measurement.key {
					count[0]++
				} else {
					count[1]++
				}
				counts[measurement.field] = count
				depth = max(depth, measurement.depth)
			}
			if !reflect.DeepEqual(counts, fixture.observed) || depth < fixture.depth {
				test.Fatal("required actual generic key/value builders or linked frames were not observed", counts, fixture.observed, depth, fixture.depth)
			}
			var excess int64
			for _, field := range []string{"input", "output"} {
				switch value := fields[field].(type) {
				case map[string]any:
					excess += observer.valueExcess(test, field, value)
				case []any:
					excess += observer.valueExcess(test, field, value)
				}
			}
			handoff.check(test, encoding, excess)
			eventBody, err = legacyNormalizationTestBytes(context.Background(), step.Events[0])
			if err != nil {
				test.Fatal("generic canonical encoder failed after native ownership release", err)
			}
		}
		if step.Complete {
			if eventBody == nil || step.Count != 1 || !bytes.Equal(AssembleCanonicalDoc(step.Envelope, [][]byte{eventBody}), wantDocument) {
				test.Fatal("generic ownership fixture changed the complete native canonical document", step.Count)
			}
			return
		}
	}
	test.Fatal("generic ownership fixture exhausted its bounded per-part progress allowance")
}

func TestLegacyNativeAnyOwnershipCanonicalHandoff(test *testing.T) {
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	key := `"` + strings.Repeat("k", 2*ConversationManifestChunkBytes+73) + `"` + padding
	otherKey := `"` + strings.Repeat("q", ConversationManifestChunkBytes+109) + `"` + padding
	value := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+91) + `"` + padding
	otherValue := `"` + strings.Repeat("y", ConversationManifestChunkBytes+137) + `"` + padding
	object := `{` + key + `:` + value + `}`
	otherObject := `{` + otherKey + `:` + otherValue + `}`
	for _, fixture := range []legacyNativeAnyOwnershipFixture{
		{name: "input-key-and-value", event: `{"kind":"tool_call","seq":7,"input":` + object + `}`, observed: map[string][2]int{"input": {1, 1}}},
		{name: "input-outer-map-merges", event: `{"kind":"tool_call","seq":7,"input":` + object + `,"INPUT":` + otherObject + `}`, observed: map[string][2]int{"input": {2, 2}}},
		{name: "input-equal-key-replaces-value-only", event: `{"kind":"tool_call","seq":7,"input":` + object + `,"INPUT":{` + key + `:` + otherValue + `}}`, observed: map[string][2]int{"input": {2, 2}}},
		{name: "input-null-value-keeps-key-allocation", event: `{"kind":"tool_call","seq":7,"input":{` + key + `:` + value + `,` + key + `:null}}`, observed: map[string][2]int{"input": {2, 1}}},
		{name: "input-numeric-value-keeps-key-allocation", event: `{"kind":"tool_call","seq":7,"input":{` + key + `:` + value + `,` + key + `:9007199254740993}}`, observed: map[string][2]int{"input": {2, 1}}},
		{name: "input-null-map-releases-old-keys-and-values", event: `{"kind":"tool_call","seq":7,"input":` + object + `,"INPUT":null,"input":` + otherObject + `}`, observed: map[string][2]int{"input": {2, 2}}},
		{name: "input-linked-object-array-frames", event: `{"kind":"tool_call","seq":7,"input":{` + key + `:[` + value + `,{` + otherKey + `:` + otherValue + `},9007199254740993,-0,1.250e-8,true,null]}}`, observed: map[string][2]int{"input": {2, 2}}, depth: 3},
		{name: "output-object-keys-and-values", event: `{"kind":"tool_result","seq":7,"output":` + object + `}`, observed: map[string][2]int{"output": {1, 1}}},
		{name: "output-object-replaces-not-merges", event: `{"kind":"tool_result","seq":7,"output":` + object + `,"OUTPUT":` + otherObject + `}`, observed: map[string][2]int{"output": {2, 2}}},
		{name: "output-object-to-array", event: `{"kind":"tool_result","seq":7,"output":` + object + `,"OUTPUT":[` + otherValue + `,` + otherObject + `]}`, observed: map[string][2]int{"output": {2, 3}}, depth: 2},
		{name: "output-array-to-object", event: `{"kind":"tool_result","seq":7,"output":[` + value + `],"OUTPUT":` + otherObject + `}`, observed: map[string][2]int{"output": {1, 2}}},
		{name: "output-array-replaces-array", event: `{"kind":"tool_result","seq":7,"output":[` + value + `],"OUTPUT":[` + otherValue + `]}`, observed: map[string][2]int{"output": {0, 2}}},
		{name: "output-short-string-releases-object-capacity", event: `{"kind":"tool_result","seq":7,"output":` + object + `,"OUTPUT":"short"}`, observed: map[string][2]int{"output": {1, 1}}},
		{name: "output-null-releases-object-capacity", event: `{"kind":"tool_result","seq":7,"output":` + object + `,"OUTPUT":null}`, observed: map[string][2]int{"output": {1, 1}}},
		{name: "output-short-string-releases-array-capacity", event: `{"kind":"tool_result","seq":7,"output":[` + value + `],"OUTPUT":"short"}`, observed: map[string][2]int{"output": {0, 1}}},
		{name: "output-null-releases-array-capacity", event: `{"kind":"tool_result","seq":7,"output":[` + value + `],"OUTPUT":null}`, observed: map[string][2]int{"output": {0, 1}}},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeAnyOwnershipCheck(test, fixture) })
	}
}

func TestLegacyNativeAnyOwnershipFinishedReplacementRetainsOldOutput(test *testing.T) {
	key := `"` + strings.Repeat("k", 2*ConversationManifestChunkBytes+73) + `"`
	value := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+91) + `"`
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	for _, replacement := range []struct {
		name string
		raw  string
	}{
		{name: "object", raw: `{` + key + `:[` + value + `]` + padding + `}`},
		{name: "array", raw: `[` + value + padding + `]`},
	} {
		test.Run(replacement.name, func(test *testing.T) {
			raw := []byte(`{"output":{` + key + `:` + value + `},"OUTPUT":` + replacement.raw + `}`)
			actual := &CIREvent{}
			metadata := newLegacyNativeObjectFields(reflect.TypeOf(*actual))
			native, err := newLegacyNativeObject(reflect.ValueOf(actual), &metadata, 3, false)
			if err != nil {
				test.Fatal("generic replacement constructor failed", err)
			}
			native.fieldLimit = 1
			native.start(0, len(raw))
			field := metadata.match("output")
			buffer := &legacySourceProofBuffer{body: raw}
			source := buffer.input()
			closingPosition := len(raw) - 2
			var frame legacyJSONValueScanner
			var previous *legacyNativeAny
			var previousValue any
			var previousHeap docRangeHeaderHeapEstimate
			observer := newLegacyNativeAnyOwnershipObserver()
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				observer.observe(test, native)
				var helper *legacyNativeAny
				if len(native.containers) != 0 {
					helper = native.containers[field]
				}
				if previous == nil && helper != nil && !native.containerActive {
					previous, previousValue, previousHeap = helper, actual.Output, native.heaps[field]
				}
				closing := previous != nil && helper != nil && helper != previous && native.containerActive && !helper.finished &&
					helper.frame != nil && helper.frame.parent == nil && helper.frame.phase == legacyNativeAnyAfter && !helper.pending &&
					helper.position <= closingPosition && closingPosition-helper.position < ConversationManifestChunkBytes
				input := &legacyNormalizationInput{ctx: context.Background(), source: source}
				if closing {
					input.workBytes = ConversationManifestChunkBytes - (closingPosition - helper.position + 1)
				}
				complete, err := native.step(context.Background(), input, &frame)
				if err != nil || input.failure != nil || input.unbounded || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
					test.Fatal("generic replacement escaped its shared native input", err, input.failure, input.workBytes, input.loads)
				}
				observer.observe(test, native)
				if !closing {
					if complete {
						test.Fatal("generic replacement never exposed the finished-but-active handoff boundary")
					}
					continue
				}
				if complete || !helper.finished || helper.frame != nil || !native.containerActive || input.workBytes != ConversationManifestChunkBytes || input.fields != 0 ||
					!reflect.DeepEqual(actual.Output, previousValue) || native.heaps[field] != previousHeap || helper.result.heap.err != nil || helper.result.excess <= 0 {
					test.Fatal("finished generic root prematurely replaced or released the old Output")
				}
				if replacement.name == "object" && helper.rootMap == nil || replacement.name == "array" && helper.rootArray == nil {
					test.Fatal("finished generic replacement lost its actual root owner")
				}
				measuredExcess := observer.valueExcess(test, "output", helper.result.value)
				if len(observer.active) != 0 || helper.result.excess != measuredExcess {
					test.Fatal("finished replacement lost its actual key/value builder provenance", helper.result.excess, measuredExcess, len(observer.active))
				}
				var pending docRangeHeaderHeapEstimate
				helper.retained(&pending)
				retained, retainedErr := native.retained()
				if pending.err != nil || retainedErr != nil || previousHeap.err != nil || previousHeap.bytes <= 0 || retained < pending.bytes+previousHeap.bytes {
					test.Fatal("finished-but-active replacement stopped charging old Output alongside its completed new root", retained, pending.bytes, previousHeap.bytes, pending.err, retainedErr)
				}
				withoutPrevious := *native
				withoutPrevious.heaps = make([]docRangeHeaderHeapEstimate, len(native.heaps), cap(native.heaps))
				copy(withoutPrevious.heaps, native.heaps)
				withoutPrevious.heaps[field] = docRangeHeaderHeapEstimate{}
				withoutBytes, withoutErr := withoutPrevious.retained()
				var fieldStorage docRangeHeaderHeapEstimate
				fieldStorage.allocation(1, int64(metadata.model.Field(metadata.fields[field].index).Type.Size()))
				previousPayload := previousHeap.bytes - fieldStorage.bytes
				if fieldStorage.err != nil || previousPayload <= 0 || withoutErr != nil || retained-withoutBytes != previousPayload {
					test.Fatal("old Output payload is not retained exactly once alongside the shared field slot", retained-withoutBytes, previousPayload, fieldStorage.err, withoutErr)
				}
				input = &legacyNormalizationInput{ctx: context.Background(), source: source}
				complete, err = native.step(context.Background(), input, &frame)
				if err != nil || input.failure != nil || complete || native.containerActive || input.fields != 1 ||
					!reflect.DeepEqual(actual.Output, helper.result.value) || native.backingExcess[field] != helper.result.excess {
					test.Fatal("next shared quantum did not transfer the completed generic root", err, input.failure, complete, input.fields)
				}
				if retained, retainedErr = native.retained(); retainedErr != nil || retained <= 0 {
					test.Fatal("replacement handoff lost retained accounting", retained, retainedErr)
				}
				withoutPrevious = *native
				withoutPrevious.heaps = make([]docRangeHeaderHeapEstimate, len(native.heaps), cap(native.heaps))
				copy(withoutPrevious.heaps, native.heaps)
				withoutPrevious.heaps[field] = docRangeHeaderHeapEstimate{}
				withoutBytes, withoutErr = withoutPrevious.retained()
				if withoutErr != nil || retained != withoutBytes {
					test.Fatal("committed replacement still charged a separate old Output heap", retained-withoutBytes, withoutErr)
				}
				return
			}
			test.Fatal("generic replacement exhausted its bounded per-part progress allowance")
		})
	}
}
