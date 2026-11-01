package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeNestedOwnershipMeasurement struct {
	capacity int
	text     string
}

type legacyNativeNestedOwnershipActive struct {
	scalar *legacyNativeScalar
	field  int
}

type legacyNativeNestedOwnershipObserver struct {
	objects      map[*legacyNativeObject]string
	active       map[*legacyNativeObject]legacyNativeNestedOwnershipActive
	measurements map[string][]legacyNativeNestedOwnershipMeasurement
}

func newLegacyNativeNestedOwnershipObserver() *legacyNativeNestedOwnershipObserver {
	return &legacyNativeNestedOwnershipObserver{
		objects:      make(map[*legacyNativeObject]string),
		active:       make(map[*legacyNativeObject]legacyNativeNestedOwnershipActive),
		measurements: make(map[string][]legacyNativeNestedOwnershipMeasurement),
	}
}

func (observer *legacyNativeNestedOwnershipObserver) observe(test *testing.T) {
	test.Helper()
	for object, path := range observer.objects {
		active, tracked := observer.active[object]
		if tracked && object.scalar != active.scalar {
			scalar := active.scalar
			if object.scalar != nil || scalar.text == nil || scalar.previous != nil || scalar.position != scalar.end || scalar.buffered != 0 {
				test.Fatal("nested scalar was replaced or released before its builder completed", path)
			}
			field := object.metadata.fields[active.field]
			decoded := object.destination.Elem().Field(field.index)
			if decoded.Kind() != reflect.String || decoded.String() != scalar.text.String() {
				test.Fatal("committed nested string no longer matches its observed builder", path, field.name)
			}
			capacity, length := scalar.text.Cap(), scalar.text.Len()
			if capacity <= length || length <= LegacyCanonicalEncodingBytes {
				test.Fatal("nested fixture did not expose spare capacity in its actual builder", path, field.name, capacity, length)
			}
			minimum, err := legacyNativeValueHeap(context.Background(), decoded)
			committed := object.heaps[active.field]
			excess := 2 * int64(capacity-length)
			if err != nil || minimum.err != nil || committed.err != nil || committed.bytes-minimum.bytes != excess || object.backingExcess[active.field] != excess {
				test.Fatal("nested native heap lost its measured builder capacity", path, field.name, committed, minimum, excess, err)
			}
			name := path + "." + field.name
			observer.measurements[name] = append(observer.measurements[name], legacyNativeNestedOwnershipMeasurement{capacity: capacity, text: scalar.text.String()})
			delete(observer.active, object)
		}
		if object.scalar != nil {
			observer.active[object] = legacyNativeNestedOwnershipActive{scalar: object.scalar, field: object.field}
		}
	}
}

func (observer *legacyNativeNestedOwnershipObserver) observeNormalizer(test *testing.T, normalizer *LegacyNormalizationDecoder) {
	test.Helper()
	if normalizer.nativeCache != nil {
		object := normalizer.nativeCache.object
		nested := reflect.ValueOf(object).Elem().FieldByName("nested")
		if nested.IsValid() {
			if nested.Type() != reflect.TypeFor[[]*legacyNativeObject]() || nested.Len() != 0 && nested.Len() != len(object.metadata.fields) {
				test.Fatal("native nested ownership storage is not indexed by parent field")
			}
			for index := 0; index < nested.Len(); index++ {
				if nested.Index(index).IsNil() {
					continue
				}
				name := object.metadata.fields[index].name
				if name == "locked" || name == "provider_metadata" {
					child := (*legacyNativeObject)(nested.Index(index).UnsafePointer())
					observer.objects[child] = name
				}
			}
		}
	}
	observer.observe(test)
}

func TestLegacyNativeNestedOwnershipScopedBuilders(test *testing.T) {
	large := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"` + strings.Repeat(" ", ConversationManifestChunkBytes+1)
	for _, fixture := range []struct {
		name        string
		field       string
		raw         string
		destination any
	}{
		{name: "locked", field: "blob", raw: `{"provider":"codex","scheme":"encrypted_content","blob":` + large + `}`, destination: &LockedBlob{}},
		{name: "provider_metadata", field: "turn_id", raw: `{"turn_id":` + large + `,"create_time":123.450e-8}`, destination: &ProviderMetadata{}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			metadata := newLegacyNativeObjectFields(reflect.TypeOf(fixture.destination).Elem())
			native, err := newLegacyNativeObject(reflect.ValueOf(fixture.destination), &metadata, 4, false)
			if err != nil {
				test.Fatal("scoped child constructor failed", err)
			}
			expected := reflect.New(metadata.model).Interface()
			if err := json.Unmarshal(raw, expected); err != nil {
				test.Fatal("installed encoding/json rejected the child fixture", err)
			}
			buffer := &legacySourceProofBuffer{body: raw}
			source := buffer.input()
			observer := newLegacyNativeNestedOwnershipObserver()
			observer.objects[native] = fixture.name
			var frame legacyJSONValueScanner
			native.start(0, len(raw))
			native.prefix = true
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				input := &legacyNormalizationInput{ctx: context.Background(), source: source}
				complete, err := native.step(context.Background(), input, &frame)
				if err != nil || input.failure != nil {
					test.Fatal("scoped child decode failed", stepIndex, err, input.failure)
				}
				observer.observe(test)
				if !complete {
					continue
				}
				measurements := observer.measurements[fixture.name+"."+fixture.field]
				if len(observer.active) != 0 || len(measurements) != 1 || !reflect.DeepEqual(fixture.destination, expected) {
					test.Fatal("scoped decode lost native semantics or actual builder provenance", len(observer.active), len(measurements))
				}
				retained, retainedErr := native.retained()
				measurement := measurements[0]
				excess, excessErr := native.backingExcessBytes(nil)
				if retainedErr != nil || excessErr != nil || retained < 2*int64(measurement.capacity) || excess != 2*int64(measurement.capacity-len(measurement.text)) {
					test.Fatal("scoped child retained accounting lost its measured allocation", retained, excess, measurement.capacity, len(measurement.text), retainedErr, excessErr)
				}
				return
			}
			test.Fatal("scoped child exhausted its bounded progress allowance")
		})
	}
}

type legacyNativeNestedOwnershipFixture struct {
	name     string
	event    string
	observed map[string]int
	retained []string
}

func legacyNativeNestedOwnershipCheck(test *testing.T, fixture legacyNativeNestedOwnershipFixture) {
	test.Helper()
	raw := legacyNativeEventDocument("2", fixture.event)
	wantDocument, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal("installed native JSON oracle rejected the ownership fixture", err)
	}
	var expected CIREvent
	if err := json.Unmarshal([]byte(fixture.event), &expected); err != nil {
		test.Fatal("installed encoding/json rejected the event fixture", err)
	}
	wantFields, err := canonicalEventJSONObject(expected)
	if err != nil {
		test.Fatal("native event cannot be projected canonically", err)
	}
	normalizer, source := legacyNativeScalarReviewStart(test, raw)
	handoff := &legacyCanonicalNativeHandoffObservation{Context: context.Background(), normalizer: normalizer}
	observer := newLegacyNativeNestedOwnershipObserver()
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	var eventBody []byte
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		observer.observeNormalizer(test, normalizer)
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, source, handoff.withContext)
		if err != nil {
			test.Fatal("native nested decode failed before its canonical handoff", stepIndex, err)
		}
		observer.observeNormalizer(test, normalizer)
		if len(step.Events) != 0 {
			if eventBody != nil || len(step.Events) != 1 || step.Events[0].Encoding == nil || step.Events[0].Sequence != expected.Seq || len(observer.active) != 0 {
				test.Fatal("unexpected canonical handoff or unmeasured child scalar", len(step.Events), len(observer.active))
			}
			encoding := step.Events[0].Encoding
			fields, ok := encoding.root.(map[string]any)
			if !ok || !reflect.DeepEqual(fields, wantFields) {
				test.Fatal("nested duplicate merge, null reset, or field omission differs from encoding/json")
			}
			for path, count := range fixture.observed {
				if actual := len(observer.measurements[path]); actual != count {
					test.Fatal("required nested builder was not observed in the native cache before handoff", path, actual, count)
				}
			}
			var excess int64
			for _, path := range fixture.retained {
				measurements := observer.measurements[path]
				if len(measurements) == 0 {
					test.Fatal("emitted nested string has no measured builder provenance", path)
				}
				measurement := measurements[len(measurements)-1]
				parent, name, _ := strings.Cut(path, ".")
				nested, ok := fields[parent].(map[string]any)
				if !ok || nested[name] != measurement.text {
					test.Fatal("emitted nested field does not match its final observed builder", path)
				}
				excess += 2 * int64(measurement.capacity-len(measurement.text))
			}
			handoff.check(test, encoding, excess)
			eventBody, err = legacyNormalizationTestBytes(context.Background(), step.Events[0])
			if err != nil {
				test.Fatal("pending nested encoder failed after its native cache was released", err)
			}
		}
		if step.Complete {
			if eventBody == nil || step.Count != 1 || !bytes.Equal(AssembleCanonicalDoc(step.Envelope, [][]byte{eventBody}), wantDocument) {
				test.Fatal("nested ownership fixture changed complete native canonical output", step.Count)
			}
			return
		}
	}
	test.Fatal("nested ownership fixture exhausted its bounded per-part progress allowance")
}

func TestLegacyNativeNestedOwnershipCanonicalHandoff(test *testing.T) {
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	blob := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"` + padding
	turnID := `"` + strings.Repeat("y", 2*ConversationManifestChunkBytes+91) + `"` + padding
	locked := `{"provider":"codex","scheme":"encrypted_content","blob":` + blob + `}`
	metadata := `{"turn_id":` + turnID + `,"create_time":123.450e-8}`
	for _, fixture := range []legacyNativeNestedOwnershipFixture{
		{
			name: "locked-blob", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `}`,
			observed: map[string]int{"locked.blob": 1}, retained: []string{"locked.blob"},
		},
		{
			name: "provider-turn-id", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `}`,
			observed: map[string]int{"provider_metadata.turn_id": 1}, retained: []string{"provider_metadata.turn_id"},
		},
		{
			name: "independent-nested-owners", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"provider_metadata":` + metadata + `}`,
			observed: map[string]int{"locked.blob": 1, "provider_metadata.turn_id": 1}, retained: []string{"locked.blob", "provider_metadata.turn_id"},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeNestedOwnershipCheck(test, fixture) })
	}
}

func TestLegacyNativeNestedOwnershipDuplicateRelease(test *testing.T) {
	padding := strings.Repeat(" ", ConversationManifestChunkBytes+1)
	giant := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"` + padding
	shorter := `"` + strings.Repeat("z", ConversationManifestChunkBytes+109) + `"` + padding
	locked := `{"provider":"codex","scheme":"encrypted_content","blob":` + giant + `}`
	metadata := `{"turn_id":` + giant + `,"create_time":123.450e-8}`
	for _, fixture := range []legacyNativeNestedOwnershipFixture{
		{
			name: "shortened-blob-releases-old-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"LOCKED":{"BLOB":"short"}}`,
			observed: map[string]int{"locked.blob": 1},
		},
		{
			name: "shortened-turn-id-releases-old-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":{"TURN_ID":"short"}}`,
			observed: map[string]int{"provider_metadata.turn_id": 1},
		},
		{
			name: "shorter-giant-blob-replaces-not-adds-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"LOCKED":{"blob":` + shorter + `}}`,
			observed: map[string]int{"locked.blob": 2}, retained: []string{"locked.blob"},
		},
		{
			name: "shorter-giant-turn-id-replaces-not-adds-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":{"turn_id":` + shorter + `}}`,
			observed: map[string]int{"provider_metadata.turn_id": 2}, retained: []string{"provider_metadata.turn_id"},
		},
		{
			name: "null-blob-string-keeps-live-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"LOCKED":{"blob":null}}`,
			observed: map[string]int{"locked.blob": 1}, retained: []string{"locked.blob"},
		},
		{
			name: "null-turn-id-string-keeps-live-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":{"turn_id":null}}`,
			observed: map[string]int{"provider_metadata.turn_id": 1}, retained: []string{"provider_metadata.turn_id"},
		},
		{
			name: "null-locked-pointer-releases-old-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"LOCKED":null,"locked":{"provider":"codex","scheme":"signature","blob":"fresh"}}`,
			observed: map[string]int{"locked.blob": 1},
		},
		{
			name: "null-metadata-pointer-releases-old-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":null,"provider_metadata":{"create_time":2}}`,
			observed: map[string]int{"provider_metadata.turn_id": 1},
		},
		{
			name: "omitted-blob-in-duplicate-struct-keeps-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"LOCKED":{"scheme":"signature"}}`,
			observed: map[string]int{"locked.blob": 1}, retained: []string{"locked.blob"},
		},
		{
			name: "omitted-turn-id-in-duplicate-struct-keeps-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":{"create_time":2}}`,
			observed: map[string]int{"provider_metadata.turn_id": 1}, retained: []string{"provider_metadata.turn_id"},
		},
		{
			name: "empty-turn-id-releases-omitted-field-capacity", event: `{"kind":"turn","seq":7,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":{"turn_id":""}}`,
			observed: map[string]int{"provider_metadata.turn_id": 1},
		},
		{
			name: "short-blob-keeps-independent-metadata-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"provider_metadata":` + metadata + `,"LOCKED":{"blob":"short"}}`,
			observed: map[string]int{"locked.blob": 1, "provider_metadata.turn_id": 1}, retained: []string{"provider_metadata.turn_id"},
		},
		{
			name: "null-metadata-keeps-independent-blob-capacity", event: `{"kind":"reasoning","seq":7,"locked":` + locked + `,"provider_metadata":` + metadata + `,"PROVIDER_METADATA":null,"provider_metadata":{"turn_id":"fresh"}}`,
			observed: map[string]int{"locked.blob": 1, "provider_metadata.turn_id": 1}, retained: []string{"locked.blob"},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeNestedOwnershipCheck(test, fixture) })
	}
}
