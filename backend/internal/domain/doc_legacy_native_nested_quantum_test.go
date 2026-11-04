package domain

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeNestedQuantumFixture[Child any] struct {
	Before string `json:"before"`
	Child  *Child `json:"child"`
	After  string `json:"after"`
}

func legacyNativeNestedQuantumObject[Child any](test *testing.T, raw string, start int) (*legacyNativeNestedQuantumFixture[Child], *legacyNativeObject, *legacySourceProofBuffer, LegacyByteSource) {
	test.Helper()
	destination := &legacyNativeNestedQuantumFixture[Child]{}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(*destination))
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal("nested quantum constructor failed", err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", start) + raw)}
	source := buffer.input()
	native.start(start, len(buffer.body))
	if len(buffer.reads) != 0 {
		test.Fatal("nested quantum constructor read source bytes")
	}
	return destination, native, buffer, source
}

func legacyNativeNestedQuantumStep(test *testing.T, native *legacyNativeObject, frame *legacyJSONValueScanner, buffer *legacySourceProofBuffer, source LegacyByteSource, remainingBytes, fields int) (bool, *legacyNormalizationInput) {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialWork := ConversationManifestChunkBytes - remainingBytes
	input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: initialWork, fields: fields}
	before, reads := native.progressPosition(), len(buffer.reads)
	complete, err := native.step(ctx, input, frame)
	if err != nil || input.failure != nil {
		test.Fatal("nested quantum rejected valid native JSON", err, input.failure)
	}
	if input.workBytes < initialWork || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
		input.fields < fields || input.fields > LegacyNormalizationStepEvents*len(native.metadata.fields) {
		test.Fatal("nested object reset or exceeded the owner's shared quantum", initialWork, input.workBytes, fields, input.fields, input.loads, input.unbounded)
	}
	if len(buffer.reads)-reads != input.loads {
		test.Fatal("child source loads escaped the owner's input", buffer.reads[reads:], input.loads)
	}
	for _, read := range buffer.reads[reads:] {
		if read[0] < 0 || read[0] >= len(buffer.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(buffer.body)-read[0]) {
			test.Fatal("nested object bypassed authenticated source-part reads", read)
		}
	}
	position := native.position
	if native.nestedActive {
		if native.field < 0 || native.field >= len(native.nested) || native.nested[native.field] == nil {
			test.Fatal("active child has no resumable native object")
		}
		child := native.nested[native.field]
		if !child.prefix || child.parentDepth != native.parentDepth+1 || child.end != native.end || child.trackPresence {
			test.Fatal("nested object lost its borrowed prefix, depth, or presence contract")
		}
		position = max(position, child.position)
		if len(child.raw) > child.metadata.rawBytes {
			test.Fatal("child retained an entire object prefix instead of bounded field scratch", len(child.raw))
		}
	}
	progress := native.progressPosition()
	if progress != position || progress < before || progress-before > remainingBytes || progress-before > input.workBytes-initialWork {
		test.Fatal("nested progress is not monotonic actual raw scanning within the remaining bytes", before, progress, position, remainingBytes, input.workBytes-initialWork)
	}
	if len(native.raw) > native.metadata.rawBytes {
		test.Fatal("owner retained a whole child object instead of bounded field scratch", len(native.raw))
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, input
}

func legacyNativeNestedQuantumBytes[Child any](test *testing.T, scalarName, small string) {
	test.Helper()
	large := `"` + strings.Repeat("x", LegacyCanonicalEncodingBytes+1) + `"`
	member := `"` + scalarName + `":` + large
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "child-first", raw: `{"child":{` + member + `},"after":"a","before":"b"}`},
		{name: "short-fields-around-child", raw: `{"before":"b","child":{` + small + `,` + member + `},"after":"a"}`},
		{name: "repeated-child-merges", raw: `{"child":{` + small + `},"before":"b","child":{` + member + `},"after":"a","child":{}}`},
		{name: "null-resets-between-children", raw: `{"child":{` + small + `},"before":"b","child":null,"child":{` + member + `},"after":"a"}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, alignment := range []struct {
				name  string
				start int
				loads int
			}{
				{name: "aligned", loads: 1},
				{name: "cross-part-open", start: ConversationManifestChunkBytes - 1, loads: 2},
			} {
				test.Run(alignment.name, func(test *testing.T) {
					actual, native, buffer, source := legacyNativeNestedQuantumObject[Child](test, fixture.raw, alignment.start)
					var expected legacyNativeNestedQuantumFixture[Child]
					if err := json.Unmarshal([]byte(fixture.raw), &expected); err != nil {
						test.Fatal("fixture is not accepted by encoding/json", err)
					}
					var frame legacyJSONValueScanner
					complete, input := legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, LegacyCanonicalEncodingBytes/2, 0)
					if complete || !native.nestedActive || input.loads != alignment.loads || actual.After != "" {
						test.Fatal("partly exhausted owner did not yield inside its unfinished child", complete, native.nestedActive, input.loads, actual.After)
					}
					child := native.nested[native.field]
					if child.position <= native.valueStart || child.position >= child.end || native.position != native.valueStart ||
						native.progressPosition() != child.position || frame.parents != child.parentDepth || frame.mode != legacyJSONValueString || frame.complete {
						test.Fatal("child string was prescanned, or its raw progress/scanner was not borrowed from the owner")
					}
					scalarIndex := child.metadata.match(scalarName)
					if scalarIndex < 0 {
						test.Fatal("nested scalar is absent from the child metadata", scalarName)
					}
					childValue := child.destination.Elem().Field(child.metadata.fields[scalarIndex].index)
					if childValue.String() != "" {
						test.Fatal("partial raw child scanning published the unfinished scalar")
					}
					position := native.progressPosition()
					complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 0, 0)
					if complete || native.progressPosition() != position || input.loads != 0 || input.fields != 0 {
						test.Fatal("exhausted byte quantum did not yield without output or source reads")
					}
					complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
					if complete || native.progressPosition() != position+1 || input.workBytes != ConversationManifestChunkBytes || input.fields != 0 ||
						native.nested[native.field] != child || actual.After != "" || childValue.String() != "" {
						test.Fatal("one remaining byte restarted the child quantum or advanced beyond its actual scan")
					}
					complete, _ = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, LegacyCanonicalEncodingBytes/2, 0)
					if complete || actual.After != "" {
						test.Fatal("short shared quanta unexpectedly completed the child or applied its following field")
					}
					complete, _ = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, ConversationManifestChunkBytes, 0)
					if !complete || native.nestedActive || native.progressPosition() != len(buffer.body) || !reflect.DeepEqual(*actual, expected) {
						test.Fatal("fresh quantum lost native merge/reset semantics or failed to finish the small wrapper", complete, native.progressPosition())
					}
				})
			}
		})
	}
}

func TestLegacyNativeNestedQuantumSharedBytes(test *testing.T) {
	test.Run("locked", func(test *testing.T) {
		legacyNativeNestedQuantumBytes[LockedBlob](test, "blob", `"provider":"codex","scheme":"signature"`)
	})
	test.Run("provider-metadata", func(test *testing.T) {
		legacyNativeNestedQuantumBytes[ProviderMetadata](test, "turn_id", `"create_time":17`)
	})
}

func legacyNativeNestedQuantumFields[Child any](test *testing.T, first, rest string) {
	test.Helper()
	raw := `{"child":{` + first + `,` + rest + `},"after":"a","before":"b"}`
	actual, native, buffer, source := legacyNativeNestedQuantumObject[Child](test, raw, 0)
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[Child]())
	limit := LegacyNormalizationStepEvents * len(metadata.fields)
	var frame legacyJSONValueScanner
	complete, input := legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, LegacyCanonicalEncodingBytes, limit-1)
	var partial legacyNativeNestedQuantumFixture[Child]
	if err := json.Unmarshal([]byte(`{"child":{`+first+`}}`), &partial); err != nil {
		test.Fatal("partial fixture is not accepted by encoding/json", err)
	}
	if complete || !native.nestedActive || input.fields != limit || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("child did not consume the owner's last field action and yield before publishing its container", complete, input.fields, limit)
	}
	child := native.nested[native.field]
	position := native.progressPosition()
	if child.phase != legacyNativeObjectAfter || native.position != native.valueStart || position != child.position ||
		position <= native.position || buffer.body[position] != ',' || frame.parents != child.parentDepth || !frame.complete {
		test.Fatal("field-limited yield did not expose actual child scan progress and the owner's completed scanner")
	}
	for _, exhausted := range []struct {
		remaining int
		fields    int
	}{
		{fields: 0},
		{remaining: LegacyCanonicalEncodingBytes, fields: LegacyNormalizationStepEvents * len(native.metadata.fields)},
	} {
		complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, exhausted.remaining, exhausted.fields)
		if complete || native.progressPosition() != position || input.loads != 0 || input.workBytes != ConversationManifestChunkBytes-exhausted.remaining ||
			input.fields != exhausted.fields || !reflect.DeepEqual(*actual, partial) {
			test.Fatal("exhausted shared quantum advanced a child, decoded fields, or published output", complete, input.workBytes, input.fields, input.loads)
		}
	}
	complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
	if complete || input.fields != 0 || child.phase != legacyNativeObjectKey || native.progressPosition() != position+1 ||
		frame.parents != native.parentDepth+1 || frame.mode != legacyJSONValueIdle || frame.complete || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("child comma did not reset the owner's scanner within its single remaining byte")
	}
	complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
	if complete || input.fields != 0 || native.progressPosition() != position+2 || frame.parents != child.parentDepth ||
		frame.mode != legacyJSONValueString || frame.complete || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("child key did not resume the same borrowed scanner without output")
	}
	complete, _ = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, ConversationManifestChunkBytes, 0)
	var expected legacyNativeNestedQuantumFixture[Child]
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal("fixture is not accepted by encoding/json", err)
	}
	if !complete || native.nestedActive || native.progressPosition() != len(buffer.body) || !reflect.DeepEqual(*actual, expected) {
		test.Fatal("fresh field quantum did not resume the native wrapper", complete, native.progressPosition())
	}
}

func TestLegacyNativeNestedQuantumSharedFieldsAndScanner(test *testing.T) {
	test.Run("locked", func(test *testing.T) {
		legacyNativeNestedQuantumFields[LockedBlob](test, `"provider":"codex"`, `"scheme":"signature","blob":"value"`)
	})
	test.Run("provider-metadata", func(test *testing.T) {
		legacyNativeNestedQuantumFields[ProviderMetadata](test, `"turn_id":"first"`, `"create_time":17`)
	})
}

func legacyNativeNestedQuantumClose[Child any](test *testing.T, members string) {
	test.Helper()
	raw := `{"before":"b","child":{` + members + `},"after":"a","child":{` + members + `}}`
	actual, native, buffer, source := legacyNativeNestedQuantumObject[Child](test, raw, 0)
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[Child]())
	limit := LegacyNormalizationStepEvents * len(metadata.fields)
	var frame legacyJSONValueScanner
	complete, input := legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, LegacyCanonicalEncodingBytes, limit-len(metadata.fields)-1)
	var partial legacyNativeNestedQuantumFixture[Child]
	if err := json.Unmarshal([]byte(`{"before":"b","child":{`+members+`}}`), &partial); err != nil {
		test.Fatal("partial fixture is not accepted by encoding/json", err)
	}
	if complete || !native.nestedActive || input.fields != limit || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("short preceding field and child members did not share the exhausted action quantum", complete, input.fields, limit)
	}
	child, pointer := native.nested[native.field], actual.Child
	position := native.progressPosition()
	if child.phase != legacyNativeObjectAfter || position != child.position || buffer.body[position] != '}' {
		test.Fatal("action-limited child did not stop at its actual unconsumed closing brace")
	}
	complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
	if complete || !native.nestedActive || input.fields != 0 || child.phase != legacyNativeObjectDone ||
		native.progressPosition() != position+1 || native.position != native.valueStart || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("child close exceeded its last byte or prematurely published the wrapper field")
	}
	complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
	if complete || native.nestedActive || input.fields != 1 || native.progressPosition() != position+2 ||
		frame.parents != native.parentDepth || frame.mode != legacyJSONValueIdle || frame.complete || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("completed child handoff did not charge the same input and return the scanner to its owner")
	}
	complete, input = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, 1, 0)
	if complete || input.fields != 0 || native.progressPosition() != position+3 || frame.parents != native.parentDepth ||
		frame.mode != legacyJSONValueString || frame.complete || !reflect.DeepEqual(*actual, partial) {
		test.Fatal("following short field did not borrow the scanner back within the remaining byte")
	}
	complete, _ = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, ConversationManifestChunkBytes, 0)
	var expected legacyNativeNestedQuantumFixture[Child]
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal("fixture is not accepted by encoding/json", err)
	}
	if !complete || native.nestedActive || native.progressPosition() != len(buffer.body) || actual.Child != pointer || !reflect.DeepEqual(*actual, expected) {
		test.Fatal("repeated non-null child lost its destination or native values after close-boundary yields", complete, native.progressPosition())
	}
}

func TestLegacyNativeNestedQuantumCloseAndRepeatedChild(test *testing.T) {
	test.Run("locked", func(test *testing.T) {
		legacyNativeNestedQuantumClose[LockedBlob](test, `"provider":"codex","scheme":"signature","blob":"value"`)
	})
	test.Run("provider-metadata", func(test *testing.T) {
		legacyNativeNestedQuantumClose[ProviderMetadata](test, `"turn_id":"first","create_time":17`)
	})
}

func TestLegacyNativeNestedQuantumNarrowOwnerFieldLimit(test *testing.T) {
	destination := &struct {
		Child *LockedBlob `json:"child"`
	}{}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(*destination))
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(`{"child":{"provider":"codex","scheme":"signature","blob":"value"}}`)}
	source := buffer.input()
	native.start(0, len(buffer.body))
	var frame legacyJSONValueScanner
	complete, input := legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, ConversationManifestChunkBytes, LegacyNormalizationStepEvents-1)
	if complete || !native.nestedActive || input.fields != LegacyNormalizationStepEvents || destination.Child == nil || destination.Child.Provider != ProviderCodex || destination.Child.Scheme != "" {
		test.Fatal("a wider child escaped its narrower owner's field-action budget", complete, input.fields, destination.Child)
	}
	complete, _ = legacyNativeNestedQuantumStep(test, native, &frame, buffer, source, ConversationManifestChunkBytes, 0)
	if !complete || destination.Child.Scheme != "signature" || destination.Child.Blob != "value" {
		test.Fatal("a fresh owner quantum did not resume the narrower-owner child", complete, destination.Child)
	}
}
