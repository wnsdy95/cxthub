package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func l04LegacyCanonicalTypedEvent(test *testing.T, raw string) CIREvent {
	test.Helper()
	var event CIREvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		test.Fatal("original typed event decoder rejected the canonical fixture", err)
	}
	return event
}

func legacyCanonicalNativePreparationDecode(test *testing.T, raw string) (*legacyNormalizationNativeEvent, CIREvent, int64) {
	test.Helper()
	var expected wire
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal("original wire decoder rejected the preparation fixture", err)
	}
	typed := l04LegacyCanonicalTypedEvent(test, raw)
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("native preparation fixture constructor failed", err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	var frame legacyJSONValueScanner
	observation := &l04LegacyCanonicalCapacityObservation{native: native.object}
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	blockActions := LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields)
	blockSteps := (len(expected.Blocks)*(len(legacyContentBlockNativeFields.fields)+1) + blockActions - 1) / blockActions
	for stepIndex := 0; stepIndex < 64+32*parts+blockSteps; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		observation.Context = ctx
		input := &legacyNormalizationInput{ctx: observation, source: source}
		event, complete, err := native.step(observation, input, &frame)
		cancel()
		if err != nil || input.failure != nil {
			test.Fatal("actual native decoding rejected the preparation fixture", stepIndex, err, input.failure)
		}
		if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields > LegacyNormalizationStepEvents*len(native.object.metadata.fields) || input.values > legacyNativeAnyValueActions {
			test.Fatal("native preparation fixture escaped existing decode quanta", stepIndex, input.workBytes, input.loads, input.fields, input.values)
		}
		for _, page := range input.pages {
			clear(page)
		}
		if !complete {
			continue
		}
		if !reflect.DeepEqual(native.decoded, expected) || !reflect.DeepEqual(event, typed) {
			test.Fatal("preparation fixture did not preserve original native decoding")
		}
		whole, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(event))
		if err != nil {
			test.Fatal("whole decoded event could not be measured", err)
		}
		whole.include(docRangeHeaderHeapEstimate{bytes: observation.excess(test, reflect.ValueOf(event), 0)})
		reported, err := native.object.backingExcessBytes(nil)
		if err != nil {
			test.Fatal("whole decoded event lost actual builder capacity", err)
		}
		for _, list := range native.object.blocks {
			if list != nil {
				if list.totalExcess < list.visibleExcess {
					test.Fatal("native block capacity-tail accounting became negative")
				}
				reported += list.totalExcess - list.visibleExcess
			}
		}
		if measured := observation.excess(test, reflect.ValueOf(event), 0); reported != measured {
			test.Fatal("whole native reported excess differs from actual builder observations", reported, measured)
		}
		legacyCanonicalNativeOwnershipBounds(test, event, native.object.heaps, observation)
		observation.eventRecords(test, native.object)
		if whole.err != nil || whole.bytes <= 0 {
			test.Fatal("whole native event retention is invalid", whole.bytes, whole.err)
		}
		return native, event, whole.bytes
	}
	test.Fatal("native preparation fixture exhausted its finite decode allowance")
	return nil, CIREvent{}, 0
}

func legacyCanonicalNativePreparationNew(test *testing.T, native *legacyNormalizationNativeEvent) *LegacyCanonicalEncoding {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	owner, err := native.takeCanonicalOwner(ctx)
	if err != nil {
		cancel()
		test.Fatal("actual decoded native event could not transfer its owner", err)
	}
	encoding, err := newLegacyCanonicalEncodingNative(ctx, owner)
	cancel()
	if err != nil || encoding == nil {
		test.Fatal("owner-backed native canonical constructor failed", err)
	}
	if encoding.nativeOwner != owner {
		test.Fatal("native constructor did not keep the transferred owner")
	}
	return encoding
}

func legacyCanonicalNativePreparationHandoff(test *testing.T, native *legacyNormalizationNativeEvent, event CIREvent) *LegacyCanonicalEncoding {
	test.Helper()
	normalizer := &LegacyNormalizationDecoder{native: native, nativeCache: native}
	var result LegacyNormalizationStep
	ctx, cancel := context.WithCancel(context.Background())
	err := normalizer.finishEvent(ctx, event, &result)
	cancel()
	if err != nil || normalizer.invalid || len(result.Events) != 1 || result.Events[0].Encoding == nil || result.Events[0].Sequence != event.Seq {
		test.Fatal("real native event did not reach the canonical factory", err, normalizer.invalid, len(result.Events))
	}
	return result.Events[0].Encoding
}

func assertLegacyCanonicalNativePreparationInitial(test *testing.T, encoding *LegacyCanonicalEncoding, whole int64) {
	test.Helper()
	if encoding.preparation == nil || encoding.nativeOwner == nil || encoding.complete || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 {
		test.Fatal("native constructor visited children or keys before Next", encoding.preparation == nil, encoding.nativeOwner == nil, encoding.complete, encoding.inspectWork, encoding.mapKeys.length())
	}
	initial := encoding.preparation.frames.at(0)
	if encoding.preparation.complete || encoding.preparation.frames.length() != 1 || initial == nil || initial.kind != 0 ||
		initial.phase != 0 || initial.depth != 2 || !initial.prepare || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) {
		test.Fatal("native constructor did not preserve one unvisited, incomplete preparation root")
	}
	retained, err := encoding.RetainedBytes()
	if err != nil || encoding.rootHeap.err != nil || encoding.rootHeap.bytes < whole || retained < encoding.rootHeap.bytes {
		test.Fatal("native constructor failed to charge the entire actual owner", whole, encoding.rootHeap.bytes, retained, err, encoding.rootHeap.err)
	}
}

func legacyCanonicalNativePreparationDrain(test *testing.T, encoding *LegacyCanonicalEncoding, whole int64) ([]byte, int) {
	test.Helper()
	owner := encoding.nativeOwner
	preparationSteps := 0
	var output []byte
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		preparation := encoding.preparation
		before := encoding.inspectWork
		keyWork := encoding.keyWork
		numberIndex, numberPosition := encoding.numbers.next, encoding.numbers.position
		ctx, cancel := context.WithCancel(context.Background())
		chunk, complete, err := encoding.Next(ctx)
		cancel()
		if err != nil {
			test.Fatal("native canonical preparation or streaming failed", stepIndex, err)
		}
		if len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes || encoding.inspectWork < before ||
			encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > LegacyCanonicalEncodingBytes {
			test.Fatal("canonical Next exceeded output/storage bounds or reset inspectWork", stepIndex, len(chunk), cap(chunk), before, encoding.inspectWork, encoding.lastStorageBytes)
		}
		if preparation != nil {
			preparationSteps++
			if (preparation.frames.length() == 0) != preparation.complete {
				test.Fatal("preparation completion did not match the actual final root pop", stepIndex, preparation.frames.length(), preparation.complete)
			}
			if len(chunk) != 0 || complete || encoding.inspectWork <= before || encoding.inspectWork-before > 1024 ||
				preparation.lastActions <= 0 || preparation.lastActions > 1024 || preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 ||
				preparation.lastStorageBytes < 0 || preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes ||
				encoding.inspectWork-before != uint64(preparation.lastActions) {
				test.Fatal("preparation Next failed to yield empty pending bounded progress", stepIndex, len(chunk), complete, encoding.inspectWork-before, preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes)
			}
		} else if encoding.inspectWork != before {
			test.Fatal("canonical output restarted completed inspection", stepIndex, before, encoding.inspectWork)
		}
		output = append(output, chunk...)
		if complete {
			if encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil ||
				encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
				encoding.rootHeap.bytes != 0 || encoding.pendingKeysHeap.bytes != 0 {
				test.Fatal("completed canonical stream retained its owner or preparation")
			}
			finalWork := encoding.inspectWork
			chunk, complete, err := encoding.Next(context.Background())
			if err != nil || !complete || len(chunk) != 0 || encoding.inspectWork != finalWork {
				test.Fatal("completed canonical stream changed on retry", complete, err, finalWork, encoding.inspectWork)
			}
			return output, preparationSteps
		}
		if owner != nil {
			retained, err := encoding.RetainedBytes()
			if err != nil || encoding.nativeOwner != owner || encoding.rootHeap.bytes < whole || retained < encoding.rootHeap.bytes {
				test.Fatal("pending canonical stream released or undercharged its whole native owner", stepIndex, whole, encoding.rootHeap.bytes, retained, err)
			}
		}
		if len(chunk) == 0 && encoding.inspectWork == before && encoding.keyWork == keyWork &&
			encoding.numbers.next == numberIndex && encoding.numbers.position == numberPosition {
			test.Fatal("pending canonical stream made no preparation, ordering, numeric, or output progress", stepIndex)
		}
	}
	test.Fatal("native canonical preparation exhausted its finite streaming allowance")
	return nil, preparationSteps
}

func legacyCanonicalNativePreparationOracle(test *testing.T, event CIREvent) []byte {
	test.Helper()
	value, err := canonicalEventJSONObject(event)
	if err != nil {
		test.Fatal("original canonical event oracle rejected the fixture", err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal("original canonical event bytes could not be marshaled", err)
	}
	return want
}

func legacyCanonicalNativePreparationCheck(test *testing.T, raw string, minimumSteps int) {
	test.Helper()
	native, _, whole := legacyCanonicalNativePreparationDecode(test, raw)
	want := legacyCanonicalNativePreparationOracle(test, l04LegacyCanonicalTypedEvent(test, raw))
	encoding := legacyCanonicalNativePreparationNew(test, native)
	assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
	actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
	if steps < minimumSteps || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
		test.Fatal("native preparation changed canonical bytes/hash or failed to yield", len(actual), len(want), steps, minimumSteps)
	}
}

func legacyCanonicalNativePreparationMap(count int, prefix string) string {
	var raw strings.Builder
	raw.WriteByte('{')
	numbers := [...]string{"0", "-0", "9007199254740993", "1e-999", "1.7976931348623157e308", "0.125"}
	for position := 0; position < count; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, "%q:%s", fmt.Sprintf("%skey-%05d", prefix, count-position), numbers[position%len(numbers)])
	}
	raw.WriteByte('}')
	return raw.String()
}

func legacyCanonicalNativePreparationBlocks(count int, text string) string {
	var raw strings.Builder
	raw.WriteByte('[')
	for position := 0; position < count; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"type":"text","text":%q}`, fmt.Sprintf("%s-%d", text, position))
	}
	raw.WriteByte(']')
	return raw.String()
}

func TestLegacyCanonicalNativePreparationDecodedInputOutputMaps(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		count  int
		prefix string
	}{
		{name: "empty", count: 0},
		{name: "singleton", count: 1},
		{name: "tiny-eight", count: 8},
		{name: "non-tiny-nine", count: 9},
		{name: "tiny-count-long-prefix", count: 8, prefix: strings.Repeat("prefix-", 40)},
		{name: "4097-keys", count: 4097},
		{name: "4097-long-prefix-keys", count: 4097, prefix: strings.Repeat("shared-prefix-", 12)},
	} {
		for _, field := range []string{"input", "output"} {
			test.Run(fixture.name+"/"+field, func(test *testing.T) {
				kind := "tool_call"
				if field == "output" {
					kind = "tool_result"
				}
				raw := fmt.Sprintf(`{"kind":%q,"seq":7,"call_id":"call","tool_name":"tool",%q:%s}`, kind, field, legacyCanonicalNativePreparationMap(fixture.count, fixture.prefix))
				minimumSteps := 1
				if fixture.count == 4097 {
					minimumSteps = 2
				}
				legacyCanonicalNativePreparationCheck(test, raw, minimumSteps)
			})
		}
	}
}

func TestLegacyCanonicalNativePreparationDecodedTypedBlocks4097(test *testing.T) {
	raw := `{"kind":"message","seq":7,"role":"assistant","blocks":` + legacyCanonicalNativePreparationBlocks(4097, "text <&> 한국🙂") + `}`
	native, event, whole := legacyCanonicalNativePreparationDecode(test, raw)
	if len(event.Blocks) != 4097 {
		test.Fatal("native fixture lost its typed block list", len(event.Blocks))
	}
	want := legacyCanonicalNativePreparationOracle(test, l04LegacyCanonicalTypedEvent(test, raw))
	encoding := legacyCanonicalNativePreparationNew(test, native)
	assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
	fields, ok := encoding.root.(map[string]any)
	blocks, typed := fields["blocks"].([]ContentBlock)
	if !ok || !typed || len(blocks) != len(event.Blocks) || &blocks[0] != &event.Blocks[0] {
		test.Fatal("native constructor expanded or copied typed blocks")
	}
	actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
	if steps < 2 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
		test.Fatal("typed block preparation changed canonical bytes/hash or did not yield", steps)
	}
}

func TestLegacyCanonicalNativePreparationDecodedScalarsAndNumbers(test *testing.T) {
	for _, token := range []string{"0", "-0", "9007199254740993", "1e-999", "1.7976931348623157e308", "0.125"} {
		test.Run("output/"+token, func(test *testing.T) {
			legacyCanonicalNativePreparationCheck(test, `{"kind":"tool_result","seq":7,"output":`+token+`}`, 1)
		})
	}
	for _, token := range []string{"123.450e-8", "1e999", "1." + strings.Repeat("0", 2*LegacyCanonicalEncodingBytes+17) + "5"} {
		test.Run(fmt.Sprintf("provider-number/%d", len(token)), func(test *testing.T) {
			legacyCanonicalNativePreparationCheck(test, `{"kind":"turn","seq":7,"provider_metadata":{"turn_id":"turn","create_time":`+token+`}}`, 1)
		})
	}
	legacyCanonicalNativePreparationCheck(test, `{"kind":"tool_result","seq":7,"output":[null,true,false,"<>&한국🙂",[-0,0.125],{"nested":9007199254740993}]}`, 1)
}

func TestLegacyCanonicalNativePreparationRetainsCapacityTailsAndOmittedFields(test *testing.T) {
	for _, kind := range []string{"message", "turn"} {
		test.Run(kind, func(test *testing.T) {
			hidden := strings.Repeat("hidden capacity payload", 32)
			raw := fmt.Sprintf(`{"kind":%q,"seq":7,"role":"user","blocks":%s,"blocks":[null],"input":%s,"output":%q,"status":%q}`,
				kind, legacyCanonicalNativePreparationBlocks(4097, hidden), legacyCanonicalNativePreparationMap(4097, "omitted-"), hidden, strings.Repeat("omitted-status", 8192))
			native, event, whole := legacyCanonicalNativePreparationDecode(test, raw)
			if len(event.Blocks) != 1 || cap(event.Blocks) < 4097 || !strings.HasPrefix(event.Blocks[:cap(event.Blocks)][4096].Text, hidden) || len(event.Input) != 4097 {
				test.Fatal("real native fixture lost its hidden capacity tail or omitted input", len(event.Blocks), cap(event.Blocks), len(event.Input))
			}
			want := legacyCanonicalNativePreparationOracle(test, l04LegacyCanonicalTypedEvent(test, raw))
			projected, err := legacyCanonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			fresh, err := newLegacyCanonicalEncodingPending(context.Background(), projected)
			if err != nil {
				test.Fatal("unowned projected retention oracle failed", err)
			}
			projectedRetained, err := fresh.RetainedBytes()
			if err != nil || whole <= projectedRetained {
				test.Fatal("fixture did not distinguish projected retention from the whole owner", whole, projectedRetained, err)
			}
			encoding := legacyCanonicalNativePreparationNew(test, native)
			assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
			actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
			if steps == 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
				test.Fatal("whole-owner retention changed visible canonical bytes/hash", steps)
			}
		})
	}
}

func TestLegacyCanonicalNativePreparationUnownedConstructorsStaySynchronous(test *testing.T) {
	raw := `{"kind":"tool_call","seq":7,"input":` + legacyCanonicalNativePreparationMap(4097, "") + `}`
	_, event, _ := legacyCanonicalNativePreparationDecode(test, raw)
	want := legacyCanonicalNativePreparationOracle(test, l04LegacyCanonicalTypedEvent(test, raw))
	for _, constructor := range []struct {
		name string
		new  func(context.Context, any) (*LegacyCanonicalEncoding, error)
	}{
		{name: "eager", new: newLegacyCanonicalEncoding},
		{name: "pending", new: newLegacyCanonicalEncodingPending},
	} {
		test.Run(constructor.name, func(test *testing.T) {
			value, err := legacyCanonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			encoding, err := constructor.new(context.Background(), value)
			if err != nil {
				test.Fatal(err)
			}
			if encoding.preparation != nil || encoding.nativeOwner != nil || encoding.inspectWork == 0 || encoding.mapKeys.length() == 0 {
				test.Fatal("unowned constructor stopped draining the common inspect machine synchronously", constructor.name, encoding.inspectWork, encoding.mapKeys.length())
			}
			actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, 0)
			if steps != 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
				test.Fatal("synchronous inspect drain changed canonical bytes/hash", constructor.name, steps)
			}
		})
	}
}

func TestLegacyCanonicalNativePreparationReplacementNativeHandoffPending(test *testing.T) {
	for _, replacement := range []string{
		`[]`,
		`[{"kind":"turn","seq":8,"role":"assistant"},{"kind":"compaction","seq":1,"replacement":[{"kind":"turn","seq":3},{"kind":"turn","seq":-1}]}]`,
	} {
		test.Run(fmt.Sprintf("replacement-bytes-%d", len(replacement)), func(test *testing.T) {
			raw := `{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":` + replacement + `}`
			native, event, whole := legacyCanonicalNativePreparationDecode(test, raw)
			if event.Replacement == nil {
				test.Fatal("replacement fixture lost the explicit non-nil boundary")
			}
			before, err := json.Marshal(event)
			if err != nil {
				test.Fatal(err)
			}
			normalized := l04LegacyCanonicalTypedEvent(test, raw)
			normalized.Replacement = canonicalEvents(normalized.Replacement)
			want := legacyCanonicalNativePreparationOracle(test, normalized)
			encoding := legacyCanonicalNativePreparationHandoff(test, native, event)
			assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
			if native.canonicalReady || !reflect.DeepEqual(native.decoded, wire{}) || !reflect.DeepEqual(encoding.nativeOwner.event, event) {
				test.Fatal("replacement handoff retained native state or changed the whole owner")
			}
			fields, ok := encoding.root.(map[string]any)
			replacement, wrapped := fields["replacement"].(*legacyCanonicalReplacement)
			if !ok || !wrapped || replacement == nil || replacement.events == nil ||
				!reflect.DeepEqual(replacement.events, event.Replacement) || cap(replacement.events) != cap(event.Replacement) ||
				!reflect.DeepEqual(replacement.order, legacyCanonicalKeyOrder{}) {
				test.Fatal("native replacement constructor expanded or ordered children before Next")
			}
			if len(event.Replacement) != 0 && &replacement.events[0] != &event.Replacement[0] {
				test.Fatal("native replacement constructor copied the decoded array")
			}
			actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
			if steps == 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
				test.Fatal("native replacement preparation changed canonical ordering, bytes, hash, or completion", steps)
			}
			after, err := json.Marshal(event)
			if err != nil || !bytes.Equal(before, after) {
				test.Fatal("native replacement preparation mutated the source event", err)
			}
		})
	}
}

func TestL04LegacyCanonicalNativePreparationReplacementChildBuilderCapacity(test *testing.T) {
	text := strings.Repeat("x", 2*LegacyCanonicalEncodingBytes+73)
	number := "1." + strings.Repeat("7", 2*LegacyCanonicalEncodingBytes+37)
	raw := fmt.Sprintf(`{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[{"kind":"reasoning","seq":2,"status":%q,"provider_metadata":{"turn_id":%q,"create_time":%s},"locked":{"provider":"codex","scheme":"encrypted_content","blob":%q}},{"kind":"message","seq":1,"role":"assistant","blocks":[{"type":"text","text":"visible"},{"type":"hidden","text":%q}],"blocks":[null],"input":{"omitted":%q}},{"kind":"tool_result","seq":3,"call_id":"call","output":{"nested":[%q]}}]}`, text, text, number, text, text, text, text)
	native, event, whole := legacyCanonicalNativePreparationDecode(test, raw)
	minimum, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(event))
	if err != nil || whole <= minimum.bytes || len(event.Replacement) != 3 ||
		len(event.Replacement[1].Blocks) != 1 || cap(event.Replacement[1].Blocks) < 2 ||
		event.Replacement[1].Blocks[:cap(event.Replacement[1].Blocks)][1].Text != text {
		test.Fatal("actual child fixture lost observed builder slack, omitted fields, or its hidden block tail", whole, minimum, err)
	}
	typed := l04LegacyCanonicalTypedEvent(test, raw)
	typed.Replacement = canonicalEvents(typed.Replacement)
	want := legacyCanonicalNativePreparationOracle(test, typed)
	before := legacyCanonicalReplacementOwnershipSnapshot(test, event)
	encoding := legacyCanonicalNativePreparationHandoff(test, native, event)
	assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
	actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
	if steps == 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) ||
		!reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, event)) {
		test.Fatal("observed child builder ownership changed original canonical bytes/hash or source backing", steps)
	}
}
