package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const eventMessagePublicMarker = "MESSAGE_PUBLIC_BODY_MARKER"
const eventMessageOpaqueMarker = "MESSAGE_OPAQUE_BODY_MARKER"

type eventMessageFixture struct {
	raw            []byte
	state          DocEventDisplayPreparation
	objects        map[ContentHash][]byte
	checkpoints    map[string]DocEventDisplayPreparation
	steps          map[string]int
	rawLoads       map[string]int
	rawBytes       map[string]int
	objectLoads    map[string]int
	maxOuter       int
	maxActive      int
	maxChild       int
	maxStepRaw     int
	maxStepLoads   int
	maxStepObjects int
	maxStepBytes   int
}

func authoritativeDocRangeEntry(test *testing.T, raw []byte) DocRangeEntry {
	test.Helper()
	entry, err := DecodeDocRangeMetadata(raw, DocEventIndex{Hash: HashContent(raw), Length: len(raw)})
	if err != nil {
		test.Fatal("authoritative range metadata", err)
	}
	return entry
}

func newEventMessageFixture(test *testing.T, raw []byte) *eventMessageFixture {
	test.Helper()
	state, err := NewDocEventDisplayPreparation(authoritativeDocRangeEntry(test, raw))
	if err != nil || state.Source.Kind != EventMessage {
		test.Fatal("message preparation", err)
	}
	return &eventMessageFixture{
		raw: bytes.Clone(raw), state: state, objects: make(map[ContentHash][]byte),
		checkpoints: make(map[string]DocEventDisplayPreparation), steps: make(map[string]int),
		rawLoads: make(map[string]int), rawBytes: make(map[string]int), objectLoads: make(map[string]int),
	}
}

func (fixture *eventMessageFixture) loadRaw(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 || length > 64<<10 || offset > len(fixture.raw)-length {
		return nil, ErrIntegrity
	}
	fixture.rawLoads[fixture.state.Phase]++
	fixture.rawBytes[fixture.state.Phase] += length
	return bytes.Clone(fixture.raw[offset : offset+length]), nil
}

func (fixture *eventMessageFixture) stored(ctx context.Context, hash ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(raw), nil
}

func (fixture *eventMessageFixture) loadObject(ctx context.Context, hash ContentHash) ([]byte, error) {
	fixture.objectLoads[fixture.state.Phase]++
	return fixture.stored(ctx, hash)
}

func eventMessageNoInlineBody(test *testing.T, raw []byte) {
	test.Helper()
	for _, marker := range []string{eventMessagePublicMarker, eventMessageOpaqueMarker} {
		if bytes.Contains(raw, []byte(marker)) {
			test.Fatalf("checkpoint contains inline body %q", marker)
		}
	}
}

func (fixture *eventMessageFixture) inputs(test *testing.T, state DocEventDisplayPreparation) DocMessageInputGeneration {
	test.Helper()
	inputs, err := DecodeDocMessageInputGeneration(state.Inputs, fixture.objects[state.Inputs])
	if err != nil {
		test.Fatal("message inputs", err)
	}
	return inputs
}

func (fixture *eventMessageFixture) restore(test *testing.T, state DocEventDisplayPreparation) DocEventDisplayPreparation {
	test.Helper()
	raw, err := state.Encode()
	if err != nil || len(raw) > 2048 {
		test.Fatalf("outer checkpoint exceeds 2 KiB: phase=%s bytes=%d err=%v", state.Phase, len(raw), err)
	}
	eventMessageNoInlineBody(test, raw)
	fixture.maxOuter = max(fixture.maxOuter, len(raw))
	restored, err := DecodeDocEventDisplayPreparation(HashContent(raw), bytes.Clone(raw))
	if err != nil || !reflect.DeepEqual(restored, state) {
		test.Fatal("outer checkpoint failed restart", state.Phase, err)
	}
	if state.Active == "" {
		return restored
	}
	active := fixture.objects[state.Active]
	if len(active) == 0 || len(active) > 64<<10 || HashContent(active) != state.Active {
		test.Fatal("active checkpoint exceeds 64 KiB or is missing", state.Phase, len(active))
	}
	eventMessageNoInlineBody(test, active)
	fixture.maxActive = max(fixture.maxActive, len(active))
	var encoded []byte
	switch state.Phase {
	case "discovery":
		var inner DocFieldDiscoveryState
		inner, err = DecodeDocFieldDiscoveryState(state.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
		if _, found := fixture.checkpoints["discovery/active"]; !found {
			fixture.checkpoints["discovery/active"] = state
		}
	case "message_inputs":
		var inner DocMessageInputState
		inner, err = DecodeDocMessageInputState(*state.Message, state.Active, active)
		if err == nil {
			encoded, err = inner.Encode(*state.Message)
		}
	case "summary_text":
		source := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: fixture.inputs(test, state)}
		var inner DocSummaryTextState
		inner, err = DecodeDocSummaryTextState(source, state.Active, active)
		if err == nil {
			encoded, err = inner.Encode(source)
		}
	case "markdown":
		var inner DocMarkdownPlanState
		inner, err = DecodeDocMarkdownPlanState(state.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
	case "message_plan":
		source := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: fixture.inputs(test, state)}
		var inner DocMessageDisplayState
		inner, err = DecodeDocMessageDisplayState(source, state.Active, active)
		if err == nil {
			encoded, err = inner.Encode(source)
		}
		if state.Child != "" {
			fixture.restoreChild(test, state, inner.Next)
		}
	case "assembly":
		builder := docEventDisplayBuilder{ctx: context.Background(), state: state, load: fixture.stored}
		var input DocDisplayAssemblyInput
		input, err = builder.assemblyInput()
		if err == nil {
			var inner DocDisplayAssemblyState
			inner, err = DecodeDocDisplayAssemblyState(input, state.Active, active)
			if err == nil {
				encoded, err = inner.Encode(input)
			}
		}
	default:
		test.Fatal("unexpected active phase", state.Phase)
	}
	if err != nil || !bytes.Equal(encoded, active) {
		test.Fatal("active checkpoint failed restart", state.Phase, err)
	}
	return restored
}

func (fixture *eventMessageFixture) restoreChild(test *testing.T, state DocEventDisplayPreparation, index int) {
	test.Helper()
	inputs := fixture.inputs(test, state)
	page, err := ReadDocMessageInputs(context.Background(), inputs, index/DocMessageInputPageEntries, fixture.stored)
	if err != nil {
		test.Fatal(err)
	}
	input := page[index%DocMessageInputPageEntries]
	raw := fixture.objects[state.Child]
	if len(raw) == 0 || len(raw) > 2048 {
		test.Fatal("single-child reference checkpoint exceeds 2 KiB", len(raw))
	}
	eventMessageNoInlineBody(test, raw)
	fixture.maxChild = max(fixture.maxChild, len(raw))
	child, err := decodeDocEventMessageChildTask(state.Inputs, input, state.Child, raw)
	if err != nil {
		test.Fatal("child failed restart", err)
	}
	encoded, err := child.encode(state.Inputs, input)
	if err != nil || !bytes.Equal(raw, encoded) {
		test.Fatal("child checkpoint changed during restart", err)
	}
	active := fixture.objects[child.Active]
	if len(active) == 0 || len(active) > 64<<10 {
		test.Fatal("child active checkpoint exceeds 64 KiB", len(active))
	}
	eventMessageNoInlineBody(test, active)
	fixture.maxActive = max(fixture.maxActive, len(active))
	switch child.Phase {
	case "markdown":
		var inner DocMarkdownPlanState
		inner, err = DecodeDocMarkdownPlanState(child.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
	case "placeholder":
		var inner DocMessagePlaceholderPlan
		inner, err = DecodeDocMessagePlaceholderPlan(child.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
	default:
		sources, failure := docEventMessageChildSources(input)
		if failure != nil {
			test.Fatal(failure)
		}
		position := map[string]int{"text": 0, "type": 1, "name": 2}[child.Phase]
		var inner DocDecodedTextState
		inner, err = DecodeDocDecodedTextState(*sources[position], child.Active, active)
		if err == nil {
			encoded, err = inner.Encode(*sources[position])
		}
	}
	if err != nil || !bytes.Equal(encoded, active) {
		test.Fatal("child active state failed restart", child.Phase, err)
	}
	key := "message_plan/" + child.Phase
	if _, found := fixture.checkpoints[key]; !found {
		fixture.checkpoints[key] = state
	}
}

func eventMessageZeroStep(test *testing.T, step DocEventDisplayStep) {
	test.Helper()
	if !reflect.DeepEqual(step, DocEventDisplayStep{}) || step.Prior() != "" || len(step.Objects()) != 0 || !reflect.DeepEqual(step.Preparation(), DocEventDisplayPreparation{}) {
		test.Fatal("failed step leaked partial work")
	}
}

func (fixture *eventMessageFixture) advance(test *testing.T) error {
	test.Helper()
	fixture.state = fixture.restore(test, fixture.state)
	phase := fixture.state.Phase
	if _, found := fixture.checkpoints[phase]; !found {
		fixture.checkpoints[phase] = fixture.state
	}
	fixture.steps[phase]++
	if fixture.steps[phase] > 32768 {
		test.Fatal("orchestration stopped progressing", phase, fixture.steps)
	}
	before, err := fixture.state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	rawBefore, loadsBefore := fixture.rawBytes[phase], fixture.objectLoads[phase]
	step, failure := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
	after, err := fixture.state.Encode()
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("step mutated prior preparation", err)
	}
	if failure != nil {
		eventMessageZeroStep(test, step)
		return failure
	}
	fixture.maxStepRaw = max(fixture.maxStepRaw, fixture.rawBytes[phase]-rawBefore)
	fixture.maxStepLoads = max(fixture.maxStepLoads, fixture.objectLoads[phase]-loadsBefore)
	if step.Prior() != HashContent(before) || fixture.maxStepRaw > 64<<10 {
		test.Fatal("step lost predecessor or exceeded 64 KiB source budget", fixture.maxStepRaw)
	}
	objects, totalBytes := step.Objects(), 0
	if len(objects) > 32 {
		test.Fatal("step exceeds 32 objects", len(objects))
	}
	for _, object := range objects {
		totalBytes += len(object.Bytes)
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash || bytes.Contains(object.Bytes, []byte(eventMessageOpaqueMarker)) {
			test.Fatal("invalid staging object or opaque body leak", phase)
		}
		if previous, found := fixture.objects[object.Hash]; found && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("immutable object rewritten")
		}
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.maxStepObjects = max(fixture.maxStepObjects, len(objects))
	fixture.maxStepBytes = max(fixture.maxStepBytes, totalBytes)
	if totalBytes > 1<<20 {
		test.Fatal("step exceeds 1 MiB staging budget", totalBytes)
	}
	fixture.state = fixture.restore(test, step.Preparation())
	next, err := fixture.state.Encode()
	if err != nil || bytes.Equal(before, next) {
		test.Fatal("successful step made no durable progress", err)
	}
	return nil
}

func (fixture *eventMessageFixture) finish(test *testing.T) DocDisplayProjectionGeneration {
	test.Helper()
	for fixture.state.Phase != "completed" {
		if err := fixture.advance(test); err != nil {
			test.Fatal("message orchestration", fixture.state.Phase, err)
		}
	}
	generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.stored)
	if err != nil || generation.Source.EventHash != HashContent(fixture.raw) || generation.Source.EventBytes != len(fixture.raw) || generation.Source.Plan != fixture.state.Plan {
		test.Fatal("publication lost source binding", err)
	}
	raw, err := generation.Encode()
	if err != nil || HashContent(raw) != fixture.state.Generation {
		test.Fatal("publication hash changed", err)
	}
	eventMessageNoInlineBody(test, raw)
	return generation
}

func (fixture *eventMessageFixture) oracle(test *testing.T) DocFindDisplayText {
	test.Helper()
	source := fixture.inputs(test, fixture.state).Source
	raw, shift := []byte("[]"), 0
	if !source.Empty {
		raw, shift = fixture.raw[source.Blocks.From:source.Blocks.To], source.Blocks.From
	}
	oracle, err := BuildDocPublicDisplay(context.Background(), raw, EventMessage, fixture.state.Source.Summary)
	if err != nil {
		test.Fatal("public blocks oracle", err)
	}
	return docFindShiftedDisplay{display: oracle, shift: shift}
}

func (fixture *eventMessageFixture) pages(test *testing.T, generation DocDisplayProjectionGeneration, oracle DocFindDisplayText) string {
	test.Helper()
	var output strings.Builder
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.stored)
		if err != nil || page.Index != index || page.From != output.Len() || page.ValidateProjection(generation.Source) != nil || strings.Contains(page.Text, eventMessageOpaqueMarker) {
			test.Fatal("invalid published page", index, err)
		}
		if oracle != nil {
			for offset, character := range page.Text {
				width := utf8.RuneLen(character)
				actual, err := page.SourceRange(offset, width)
				want, failure := oracle.SourceRange(page.From+offset, width)
				if err != nil || failure != nil || actual != want || actual.From < 0 || actual.To > len(fixture.raw) {
					test.Fatalf("original rune span changed: page=%d offset=%d actual=%v want=%v errors=%v/%v", index, offset, actual, want, err, failure)
				}
			}
		}
		output.WriteString(page.Text)
	}
	if output.Len() != generation.Source.TextBytes || oracle != nil && output.String() != oracle.Text() {
		test.Fatalf("display differs: got=%q bytes=%d expected_bytes=%d", output.String(), output.Len(), generation.Source.TextBytes)
	}
	return output.String()
}

func TestDocEventMessageAuthoritativeModesAndOriginalRuneSpans(test *testing.T) {
	for _, scenario := range []struct {
		name string
		body string
	}{
		{"ordinary", `{"kind":"message","blocks":[{"type":"text","text":"# **MESSAGE_PUBLIC_BODY_MARKER 한글🙂** [label](https://hidden.test)"},{"type":"image","name":"**literal** [link](hidden)\n🙂"}]}`},
		{"compact", `{"kind":"message","compact_summary":true,"blocks":[{"text":"**MESSAGE_PUBLIC_BODY_MARKER first"},{},{"type":"image","text":"last**"}]}`},
		{"seed_branch", `{"kind":"message","blocks":[{"type":"text","text":" \ufeff[cxt seed] Branch-switch context: MESSAGE_PUBLIC_BODY_MARKER"},{"text":"**한글🙂**"}]}`},
		{"seed_resumed", `{"kind":"message","blocks":[{}, {"text":"[cxt] This session was resumed from a branch context seed. MESSAGE_PUBLIC_BODY_MARKER"},{"text":"**한글🙂**"}]}`},
		{"root_last_blocks", `{"kind":"message","opaque":{"blocks":[{"type":"text","text":"MESSAGE_OPAQUE_BODY_MARKER"}]},"blocks":[{"type":"text","text":"discarded"}],"bl\u006fcks":[{"type":"text","text":"MESSAGE_PUBLIC_BODY_MARKER"}]}`},
		{"nested_decoys", `{"blocks":[{"opaque":{"type":"text","text":"MESSAGE_OPAQUE_BODY_MARKER","name":"MESSAGE_OPAQUE_BODY_MARKER"},"type":"image","name":"MESSAGE_PUBLIC_BODY_MARKER"},{"type":"image","type":"te\u0078t","text":"old","te\u0078t":"**한글\uD83D\uDE42**"}],"kind":"message"}`},
		{"escaped_unicode", `{"kind":"message","blocks":[{"type":"text","text":"# **MESSAGE_PUBLIC_BODY_MARKER \uD55C\uAE00\uD83D\uDE42**\n\n- \uD800 \uDC00 e\u0301 \\ \"quoted\""},{"type":"**\uD55C\uAE00**","name":"[\uD83D\uDE42](literal)"}]}`},
		{"missing", `{"kind":"message","opaque":{"blocks":[{"text":"MESSAGE_OPAQUE_BODY_MARKER"}]}}`},
		{"null", `{"kind":"message","blocks":null}`},
		{"empty", `{"kind":"message","blocks":[]}`},
		{"text_missing_null_empty", `{"kind":"message","blocks":[{"type":"text"},{"type":"text","text":null},{"type":"text","text":""},{"type":"text","text":" \r\n\t "}]}`},
		{"placeholder_missing_null_empty", `{"kind":"message","blocks":[{},{"type":null,"name":null},{"type":"","name":""},{"type":"**literal**","name":"` + "```" + `\n**not markdown**\n` + "```" + `"}]}`},
		{"compact_empty_slots", `{"kind":"message","compact_summary":true,"blocks":[{}, {"text":null},{"text":""},{"text":" \r\n\t "}]}`},
		{"compact_missing", `{"kind":"message","compact_summary":true}`},
		{"compact_null", `{"kind":"message","compact_summary":true,"blocks":null}`},
		{"compact_empty", `{"kind":"message","compact_summary":true,"blocks":[]}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(" \r\n" + scenario.body + "\t ")
			entry := authoritativeDocRangeEntry(test, raw)
			fixture := newEventMessageFixture(test, raw)
			if fixture.state.Source.Summary != (entry.CompactSummary || entry.SeedSummary) || strings.HasPrefix(scenario.name, "seed_") && (!entry.SeedSummary || entry.CompactSummary) || scenario.name == "compact" && (!entry.CompactSummary || entry.SeedSummary) {
				test.Fatal("authoritative compact/seed metadata did not select summary", entry)
			}
			generation := fixture.finish(test)
			got := fixture.pages(test, generation, fixture.oracle(test))
			whole, err := BuildDocEventDisplay(context.Background(), raw)
			if err != nil || got != whole.Text() {
				test.Fatal("whole-event display parity", got, err)
			}
			if fixture.state.Source.Summary != (fixture.steps["summary_text"] > 0) || fixture.state.Source.Summary == (fixture.steps["message_plan"] > 0) || fixture.rawLoads["markdown"] != 0 || fixture.rawLoads["assembly"] != 0 {
				test.Fatal("message used wrong route or reloaded raw after planning", fixture.steps, fixture.rawLoads)
			}
		})
	}
}

func TestDocEventMessageIndependentMarkdownVersusSummaryJoinedOnce(test *testing.T) {
	for _, text := range [][]string{
		{"**MESSAGE_PUBLIC_BODY_MARKER first", "last**"},
		{"```go", "**MESSAGE_PUBLIC_BODY_MARKER literal**", "```", "**tail**"},
	} {
		var displays []string
		for _, summary := range []bool{false, true} {
			blocks := make([]map[string]any, len(text))
			for index, value := range text {
				blocks[index] = map[string]any{"type": "text", "text": value}
			}
			raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": summary, "blocks": blocks})
			if err != nil {
				test.Fatal(err)
			}
			fixture := newEventMessageFixture(test, raw)
			generation := fixture.finish(test)
			displays = append(displays, fixture.pages(test, generation, fixture.oracle(test)))
		}
		if displays[0] == displays[1] {
			test.Fatal("independent and LF-joined Markdown were parsed identically", displays)
		}
	}
}

func TestDocEventMessageLegacyReasoningCanonicalCompatibility(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"legacy **한글🙂**"}`)
	entry := authoritativeDocRangeEntry(test, raw)
	state, err := NewDocEventDisplayPreparation(entry)
	if err != nil {
		test.Fatal(err)
	}
	oldSourceJSON := []byte(fmt.Sprintf(`{"empty_reasoning":false,"event_bytes":%d,"event_hash":%q,"kind":"reasoning","profile":"public-event-display-v1","version":1}`, len(raw), entry.Hash))
	oldInitialPrepJSON := []byte(fmt.Sprintf(`{"phase":"discovery","source":%s,"version":1}`, oldSourceJSON))
	encodedSource, err := state.Source.Encode()
	if err != nil || !bytes.Equal(encodedSource, oldSourceJSON) {
		test.Fatalf("legacy source bytes rewritten: got=%s want=%s err=%v", encodedSource, oldSourceJSON, err)
	}
	actualHash, err := state.Source.Hash()
	if err != nil || actualHash != HashContent(oldSourceJSON) {
		test.Fatal("legacy source hash rewritten", actualHash, err)
	}
	encoded, err := state.Encode()
	if err != nil || !bytes.Equal(encoded, oldInitialPrepJSON) || HashContent(encoded) != HashContent(oldInitialPrepJSON) {
		test.Fatalf("legacy preparation bytes/hash rewritten: got=%s want=%s err=%v", encoded, oldInitialPrepJSON, err)
	}
	restored, err := DecodeDocEventDisplayPreparation(HashContent(oldInitialPrepJSON), oldInitialPrepJSON)
	if err != nil || !reflect.DeepEqual(restored, state) {
		test.Fatal("legacy preparation cannot be decoded", err)
	}
	for _, key := range []string{`"summary"`, `"message"`, `"inputs"`, `"child"`} {
		if bytes.Contains(encoded, []byte(key)) {
			test.Fatal("optional message field changed legacy checkpoint", key)
		}
	}
	test.Logf("unchanged_legacy_source_hash=%s unchanged_legacy_preparation_hash=%s", actualHash, HashContent(encoded))
}

func TestDocEventMessageNullArrayElementsMatchWholeEventOracle(test *testing.T) {
	for _, blocks := range []string{`[null]`, `[{"type":"text","text":"visible"},null]`, `[null,{"type":"text","text":"visible"}]`} {
		for _, compact := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/summary=%t", blocks, compact), func(test *testing.T) {
				raw := []byte(fmt.Sprintf(`{"kind":"message","compact_summary":%t,"blocks":%s}`, compact, blocks))
				oracle, wantErr := BuildDocEventDisplay(context.Background(), raw)
				fixture := newEventMessageFixture(test, raw)
				var actualErr error
				for fixture.state.Phase != "completed" && actualErr == nil {
					actualErr = fixture.advance(test)
				}
				if !errors.Is(actualErr, wantErr) || (actualErr == nil) != (wantErr == nil) {
					test.Fatalf("null-element parity gap: bounded=%v whole_event=%v", actualErr, wantErr)
				}
				if wantErr == nil {
					fixture.pages(test, fixture.finish(test), oracle)
				} else if oracle != nil {
					test.Fatal("oracle leaked partial display", wantErr)
				}
			})
		}
	}
}

func eventMessageRestartBody(summary bool) []byte {
	return []byte(fmt.Sprintf(`{"kind":"message","compact_summary":%t,"opaque":{"blocks":"%s"},"blocks":[{"type":"text","text":"# **MESSAGE_PUBLIC_BODY_MARKER 한글\uD83D\uDE42**"},{"type":"**image**","name":"[MESSAGE_PUBLIC_BODY_MARKER](literal)"},{"type":"text","text":"tail"}]}`, summary, strings.Repeat(eventMessageOpaqueMarker, 3000)))
}

func (fixture *eventMessageFixture) primitiveChildCheckpoints(test *testing.T) {
	test.Helper()
	primitive := newEventMessageFixture(test, fixture.raw)
	for primitive.state.Phase != "message_plan" {
		if err := primitive.advance(test); err != nil {
			test.Fatal(err)
		}
	}
	for transitions := 0; primitive.state.Phase == "message_plan"; transitions++ {
		if transitions > 128 {
			test.Fatal("single-child checkpoint fixture did not complete")
		}
		builder := docEventDisplayBuilder{ctx: context.Background(), state: primitive.state, load: primitive.stored}
		if err := builder.messagePlan(primitive.loadRaw, &docMessageInputReader{}); err != nil {
			test.Fatal("single-child checkpoint fixture", err)
		}
		if len(builder.objects) > 32 || builder.bytes > 1<<20 {
			test.Fatal("single-child fixture exceeded primitive budget")
		}
		for _, object := range builder.objects {
			primitive.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		primitive.state = primitive.restore(test, builder.state)
	}
	for phase, state := range primitive.checkpoints {
		if strings.HasPrefix(phase, "message_plan/") {
			fixture.checkpoints[phase] = state
		}
	}
	for hash, raw := range primitive.objects {
		fixture.objects[hash] = bytes.Clone(raw)
	}
}

func TestDocEventMessageRestartEveryParentAndSingleChildPhase(test *testing.T) {
	for _, summary := range []bool{false, true} {
		test.Run(fmt.Sprintf("summary=%t", summary), func(test *testing.T) {
			fixture := newEventMessageFixture(test, eventMessageRestartBody(summary))
			generation := fixture.finish(test)
			oracle := fixture.oracle(test)
			fixture.pages(test, generation, oracle)
			required := []string{"discovery", "discovery/active", "message_inputs", "assembly"}
			if summary {
				required = append(required, "summary_text", "markdown")
			} else {
				required = append(required, "message_plan")
			}
			for _, phase := range required {
				state, found := fixture.checkpoints[phase]
				if !found {
					test.Fatal("restart phase not exercised", phase)
				}
				test.Run(phase, func(test *testing.T) {
					encoded, err := state.Encode()
					if err != nil {
						test.Fatal(err)
					}
					persistedObjects, err := json.Marshal(fixture.objects)
					if err != nil {
						test.Fatal(err)
					}
					resumed := newEventMessageFixture(test, fixture.raw)
					if json.Unmarshal(persistedObjects, &resumed.objects) != nil {
						test.Fatal("object store did not survive restart")
					}
					resumed.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), bytes.Clone(encoded))
					if err != nil {
						test.Fatal(err)
					}
					resumedGeneration := resumed.finish(test)
					resumed.pages(test, resumedGeneration, oracle)
					if resumed.state.Generation != fixture.state.Generation || resumed.state.Plan != fixture.state.Plan {
						test.Fatal("restart changed immutable publication", phase)
					}
					if phase == "assembly" || phase == "markdown" {
						for _, count := range resumed.rawLoads {
							if count != 0 {
								test.Fatal("late restart reread raw event", phase, resumed.rawLoads)
							}
						}
					}
				})
			}
		})
	}
}

func TestDocEventMessagePrimitiveChildCheckpointCompatibility(test *testing.T) {
	fixture := newEventMessageFixture(test, eventMessageRestartBody(false))
	fixture.finish(test)
	fixture.primitiveChildCheckpoints(test)
	oracle := fixture.oracle(test)
	for _, phase := range []string{"text", "markdown", "type", "name", "placeholder"} {
		test.Run(phase, func(test *testing.T) {
			state, found := fixture.checkpoints["message_plan/"+phase]
			if !found {
				test.Fatal("primitive compatibility phase not exercised", phase)
			}
			resumed := newEventMessageFixture(test, fixture.raw)
			for hash, raw := range fixture.objects {
				resumed.objects[hash] = bytes.Clone(raw)
			}
			resumed.state = resumed.restore(test, state)
			generation := resumed.finish(test)
			resumed.pages(test, generation, oracle)
			if resumed.state.Generation != fixture.state.Generation {
				test.Fatal("primitive checkpoint cannot resume with batched production API", phase)
			}
		})
	}
}

func TestDocEventMessageActualLargeChildCheckpointsRestart(test *testing.T) {
	text := "**" + eventMessagePublicMarker + " " + strings.Repeat("a🙂", 31000)
	blockType := "**" + strings.Repeat("T", 150000) + "**"
	name := "[" + strings.Repeat("N🙂", 31000) + "](literal)"
	raw, err := json.Marshal(map[string]any{
		"kind": "message", "opaque": strings.Repeat(eventMessageOpaqueMarker, 3000),
		"blocks": []map[string]any{{"type": "text", "text": text}, {"type": blockType, "name": name}},
	})
	if err != nil || len(text) < 150000 || len(blockType) < 150000 || len(name) < 150000 {
		test.Fatal("large text/type/name fixture", err)
	}
	type durableChildCheckpoint struct {
		state   []byte
		objects []byte
	}
	observed := make(map[string]durableChildCheckpoint)
	fixture := newEventMessageFixture(test, raw)
	for fixture.state.Phase != "completed" {
		if err := fixture.advance(test); err != nil {
			test.Fatal("actual large-child orchestration", err)
		}
		if fixture.state.Child == "" {
			continue
		}
		var child struct {
			Phase string `json:"phase"`
		}
		if json.Unmarshal(fixture.objects[fixture.state.Child], &child) != nil || child.Phase == "" {
			test.Fatal("actual child checkpoint has no phase")
		}
		if _, found := observed[child.Phase]; found {
			continue
		}
		encoded, err := fixture.state.Encode()
		if err != nil {
			test.Fatal(err)
		}
		objects, err := json.Marshal(fixture.objects)
		if err != nil {
			test.Fatal(err)
		}
		observed[child.Phase] = durableChildCheckpoint{state: bytes.Clone(encoded), objects: objects}
	}
	generation := fixture.finish(test)
	want := text + "\n\n[" + blockType + ": " + name + "]"
	if got := fixture.pages(test, generation, nil); got != want {
		test.Fatal("large unclosed Markdown or literal placeholder changed", len(got), len(want))
	}
	fixture.report(test, generation)
	for _, phase := range []string{"text", "markdown", "type", "name"} {
		if _, found := observed[phase]; !found {
			test.Fatal("production StepDocEventDisplay did not persist large child phase", phase)
		}
	}
	for phase, checkpoint := range observed {
		test.Run(phase, func(test *testing.T) {
			resumed := newEventMessageFixture(test, raw)
			if err := json.Unmarshal(checkpoint.objects, &resumed.objects); err != nil {
				test.Fatal(err)
			}
			resumed.state, err = DecodeDocEventDisplayPreparation(HashContent(checkpoint.state), bytes.Clone(checkpoint.state))
			if err != nil {
				test.Fatal(err)
			}
			actual := resumed.finish(test)
			if actual.Source != generation.Source || resumed.state.Generation != fixture.state.Generation || resumed.pages(test, actual, nil) != want {
				test.Fatal("actual persisted child changed after process-style restart", phase)
			}
			test.Logf("actual_production_child_phase=%s serialized_checkpoint_bytes=%d serialized_store_bytes=%d", phase, len(checkpoint.state), len(checkpoint.objects))
		})
	}
	if _, found := observed["placeholder"]; !found {
		test.Log("placeholder completes atomically inside production batches; primitive checkpoint compatibility is tested separately")
	}
}

func TestDocEventMessageLoadErrorsAndCancellationAreAtomic(test *testing.T) {
	failure := errors.New("message source interrupted")
	for _, summary := range []bool{false, true} {
		fixture := newEventMessageFixture(test, eventMessageRestartBody(summary))
		fixture.finish(test)
		for phase, state := range fixture.checkpoints {
			test.Run(fmt.Sprintf("summary=%t/%s", summary, phase), func(test *testing.T) {
				before, err := state.Encode()
				if err != nil {
					test.Fatal(err)
				}
				calls := 0
				_, err = StepDocEventDisplay(context.Background(), state,
					func(_ context.Context, offset, length int) ([]byte, error) {
						calls++
						return bytes.Clone(fixture.raw[offset : offset+length]), nil
					},
					func(ctx context.Context, hash ContentHash) ([]byte, error) { calls++; return fixture.stored(ctx, hash) })
				if err != nil || calls == 0 {
					test.Fatal("atomicity control step failed", calls, err)
				}
				for target := 0; target <= calls; target++ {
					for _, interrupt := range []string{"error", "cancel", "corrupt"} {
						ctx, cancel := context.WithCancel(context.Background())
						count := 0
						if target == 0 {
							cancel()
						}
						intercept := func(raw []byte) ([]byte, error) {
							count++
							if count != target {
								return raw, nil
							}
							switch interrupt {
							case "error":
								return nil, failure
							case "cancel":
								cancel()
								return raw, nil
							default:
								return raw[:len(raw)-1], nil
							}
						}
						step, err := StepDocEventDisplay(ctx, state,
							func(_ context.Context, offset, length int) ([]byte, error) {
								return intercept(bytes.Clone(fixture.raw[offset : offset+length]))
							},
							func(_ context.Context, hash ContentHash) ([]byte, error) {
								return intercept(bytes.Clone(fixture.objects[hash]))
							})
						cancel()
						want := failure
						if target == 0 || interrupt == "cancel" {
							want = context.Canceled
						} else if interrupt == "corrupt" {
							want = ErrIntegrity
						}
						if !errors.Is(err, want) || count != target {
							test.Fatalf("interruption not atomic: load=%d/%d mode=%s calls=%d err=%v want=%v", target, calls, interrupt, count, err, want)
						}
						eventMessageZeroStep(test, step)
						after, err := state.Encode()
						if err != nil || !bytes.Equal(before, after) {
							test.Fatal("interrupted step mutated checkpoint", err)
						}
					}
				}
			})
		}
		loads := 0
		_, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return fixture.stored(ctx, hash)
		})
		if err != nil || loads == 0 {
			test.Fatal("publication control failed", err)
		}
		for target := 0; target <= loads; target++ {
			for _, cancelLoad := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				count := 0
				if target == 0 {
					cancel()
				}
				result, err := ValidateDocEventDisplayPublication(ctx, fixture.state, func(_ context.Context, hash ContentHash) ([]byte, error) {
					count++
					if count == target {
						if !cancelLoad {
							return nil, failure
						}
						cancel()
					}
					return bytes.Clone(fixture.objects[hash]), nil
				})
				cancel()
				want := failure
				if target == 0 || cancelLoad {
					want = context.Canceled
				}
				if !errors.Is(err, want) || count != target || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) {
					test.Fatal("publication exposed partial result", summary, target, cancelLoad, count, err)
				}
			}
		}
	}
}

func TestDocEventMessageCorruptMetadataInputsAndCursors(test *testing.T) {
	for _, summary := range []bool{false, true} {
		fixture := newEventMessageFixture(test, eventMessageRestartBody(summary))
		fixture.finish(test)
		for phase, state := range fixture.checkpoints {
			test.Run(fmt.Sprintf("summary=%t/%s", summary, phase), func(test *testing.T) {
				mutations := []func(*DocEventDisplayPreparation){
					func(candidate *DocEventDisplayPreparation) {
						candidate.Source.EventHash = HashContent([]byte("unrelated event"))
					},
					func(candidate *DocEventDisplayPreparation) { candidate.Source.EventBytes++ },
					func(candidate *DocEventDisplayPreparation) { candidate.Source.Kind = EventReasoning },
					func(candidate *DocEventDisplayPreparation) { candidate.Source.Summary = !candidate.Source.Summary },
				}
				if state.Phase == "discovery" {
					mutations = nil
				}
				for _, mutate := range mutations {
					candidate := state
					mutate(&candidate)
					step, err := StepDocEventDisplay(context.Background(), candidate, fixture.loadRaw, fixture.stored)
					if !errors.Is(err, ErrIntegrity) {
						test.Fatal("metadata crossed checkpoint binding", err)
					}
					eventMessageZeroStep(test, step)
				}
				if state.Active != "" {
					var corrupt map[string]json.RawMessage
					if json.Unmarshal(fixture.objects[state.Active], &corrupt) != nil {
						test.Fatal("active checkpoint is not JSON")
					}
					if phase == "assembly" {
						corrupt["cursor"] = json.RawMessage(`{"version":1,"block":-1,"part":-1,"byte":-1,"text_bytes":-1}`)
					} else {
						corrupt["version"] = json.RawMessage("99")
					}
					encoded, err := canonicalJSON(corrupt)
					if err != nil {
						test.Fatal(err)
					}
					candidate := state
					candidate.Active = HashContent(encoded)
					fixture.objects[candidate.Active] = encoded
					step, err := StepDocEventDisplay(context.Background(), candidate, fixture.loadRaw, fixture.stored)
					if !errors.Is(err, ErrIntegrity) {
						test.Fatal("rehash concealed corrupt active cursor/state", err)
					}
					eventMessageZeroStep(test, step)
				}
				if state.Inputs != "" {
					inputs := fixture.inputs(test, state)
					inputs.Source.Summary = !inputs.Source.Summary
					encoded, err := inputs.Encode()
					if err != nil {
						test.Fatal(err)
					}
					candidate := state
					candidate.Inputs = HashContent(encoded)
					fixture.objects[candidate.Inputs] = encoded
					step, err := StepDocEventDisplay(context.Background(), candidate, fixture.loadRaw, fixture.stored)
					if !errors.Is(err, ErrIntegrity) {
						test.Fatal("summary mode crossed rehashed message inputs", err)
					}
					eventMessageZeroStep(test, step)
				}
				if state.Child != "" {
					var child map[string]json.RawMessage
					if json.Unmarshal(fixture.objects[state.Child], &child) != nil {
						test.Fatal("child checkpoint is not JSON")
					}
					child["block"] = json.RawMessage("1200")
					encoded, err := canonicalJSON(child)
					if err != nil {
						test.Fatal(err)
					}
					candidate := state
					candidate.Child = HashContent(encoded)
					fixture.objects[candidate.Child] = encoded
					step, err := StepDocEventDisplay(context.Background(), candidate, fixture.loadRaw, fixture.stored)
					if !errors.Is(err, ErrIntegrity) {
						test.Fatal("child cursor crossed selected input", err)
					}
					eventMessageZeroStep(test, step)
				}
			})
		}
		for _, body := range []string{`{"kind":"message","blocks":false}`, `{"kind":"message","blocks":[7]}`, `{"kind":"message","blocks":[{"type":"text","text":false}]}`, `{"kind":"message","blocks":[{"type":false}]}`, `{"kind":"message","blocks":[{"type":"image","name":false}]}`} {
			invalid := newEventMessageFixture(test, []byte(`{"kind":"message","blocks":[]}`))
			invalid.raw = []byte(fmt.Sprintf(`%s,"compact_summary":%t}`, body[:len(body)-1], summary))
			invalid.state.Source.EventHash, invalid.state.Source.EventBytes, invalid.state.Source.Summary = HashContent(invalid.raw), len(invalid.raw), summary
			oracle, wantErr := BuildDocEventDisplay(context.Background(), invalid.raw)
			var failure error
			for invalid.state.Phase != "completed" && failure == nil {
				failure = invalid.advance(test)
			}
			if !errors.Is(failure, wantErr) || (failure == nil) != (wantErr == nil) {
				test.Fatal("block metadata differs from whole-event oracle", body, summary, failure, wantErr)
			}
			if wantErr == nil {
				invalid.pages(test, invalid.finish(test), oracle)
			}
		}
	}
}

func TestDocEventMessageSourceKindSummaryAndPlanCannotCross(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"MESSAGE_PUBLIC_BODY_MARKER **first"},{"type":"text","text":"last**"}]}`)
	ordinary := newEventMessageFixture(test, raw)
	ordinary.finish(test)
	summary := newEventMessageFixture(test, raw)
	entry := authoritativeDocRangeEntry(test, raw)
	entry.CompactSummary = true
	var err error
	summary.state, err = NewDocEventDisplayPreparation(entry)
	if err != nil || !summary.state.Source.Summary {
		test.Fatal("authoritatively forced summary preparation", err)
	}
	summary.finish(test)
	other := newEventMessageFixture(test, bytes.Replace(raw, []byte("first"), []byte("other"), 1))
	other.finish(test)
	ordinaryHash, _ := ordinary.state.Source.Hash()
	summaryHash, _ := summary.state.Source.Hash()
	if ordinaryHash == summaryHash || ordinary.state.Plan == summary.state.Plan || ordinary.state.Generation == summary.state.Generation {
		test.Fatal("summary mode omitted from hashed identity")
	}
	for _, donor := range []*eventMessageFixture{summary, other} {
		for hash, body := range donor.objects {
			ordinary.objects[hash] = bytes.Clone(body)
		}
		for _, mutate := range []func(*DocEventDisplayPreparation){
			func(candidate *DocEventDisplayPreparation) { candidate.Source = donor.state.Source },
			func(candidate *DocEventDisplayPreparation) { candidate.Inputs = donor.state.Inputs },
			func(candidate *DocEventDisplayPreparation) { candidate.Plan = donor.state.Plan },
			func(candidate *DocEventDisplayPreparation) { candidate.Generation = donor.state.Generation },
			func(candidate *DocEventDisplayPreparation) { candidate.Source.Kind = EventReasoning },
		} {
			candidate := ordinary.state
			mutate(&candidate)
			generation, err := ValidateDocEventDisplayPublication(context.Background(), candidate, ordinary.stored)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
				test.Fatal("publication accepted crossed source/kind/summary/plan", err)
			}
		}
	}
	for phase, state := range ordinary.checkpoints {
		generation, err := ValidateDocEventDisplayPublication(context.Background(), state, ordinary.stored)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
			test.Fatal("partial preparation became publishable", phase, err)
		}
	}
}

func (fixture *eventMessageFixture) report(test *testing.T, generation DocDisplayProjectionGeneration) int {
	test.Helper()
	totalSteps, rawLoads, rawBytes, objectLoads := 0, 0, 0, 0
	for phase, count := range fixture.steps {
		totalSteps += count
		rawLoads += fixture.rawLoads[phase]
		rawBytes += fixture.rawBytes[phase]
		objectLoads += fixture.objectLoads[phase]
	}
	test.Logf("event_bytes=%d display_bytes=%d pages=%d total_steps=%d phases=%v raw_loads=%d raw_bytes=%d raw_loads_by_phase=%v raw_bytes_by_phase=%v object_loads=%d object_loads_by_phase=%v max_step_object_loads=%d max_step_raw=%d max_checkpoint=%d max_active=%d max_child=%d max_step_objects=%d max_step_bytes=%d", len(fixture.raw), generation.Source.TextBytes, generation.Pages.Count, totalSteps, fixture.steps, rawLoads, rawBytes, fixture.rawLoads, fixture.rawBytes, objectLoads, fixture.objectLoads, fixture.maxStepLoads, fixture.maxStepRaw, fixture.maxOuter, fixture.maxActive, fixture.maxChild, fixture.maxStepObjects, fixture.maxStepBytes)
	return totalSteps
}

func (fixture *eventMessageFixture) latePage(test *testing.T, generation DocDisplayProjectionGeneration, want string) {
	test.Helper()
	loads := 0
	page, err := ReadDocDisplayProjectionPage(context.Background(), generation, generation.Pages.Count-1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return fixture.stored(ctx, hash)
	})
	if err != nil || page.From <= len(want)/2 || page.Text != want[page.From:] || loads > 5 {
		test.Fatal("late-page path scanned preceding pages or changed text", loads, err)
	}
	test.Logf("late_page=%d late_page_from=%d late_page_object_loads=%d", page.Index, page.From, loads)
	for target := 1; target <= loads; target++ {
		failure, count := errors.New("late-page object failure"), 0
		result, err := ReadDocDisplayProjectionPage(context.Background(), generation, generation.Pages.Count-1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			count++
			if count == target {
				return nil, failure
			}
			return fixture.stored(ctx, hash)
		})
		if !errors.Is(err, failure) || !reflect.DeepEqual(result, DocDisplayPage{}) {
			test.Fatal("late-page load error exposed partial result", target, err)
		}
	}
}

func TestDocEventMessageLargeSinglePublicBlockAndOpaqueBounded(test *testing.T) {
	public := "**" + eventMessagePublicMarker + " " + strings.Repeat("a🙂", 31000) + "**"
	if len(public) < 150000 {
		test.Fatal("fixture no longer has one actual 150k public block")
	}
	for _, summary := range []bool{false, true} {
		test.Run(fmt.Sprintf("summary=%t", summary), func(test *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"kind": "message", "compact_summary": summary,
				"opaque": map[string]any{"text": strings.Repeat(eventMessageOpaqueMarker, 6000)},
				"blocks": []map[string]any{{"type": "text", "text": public}},
			})
			if err != nil {
				test.Fatal(err)
			}
			fixture := newEventMessageFixture(test, raw)
			generation := fixture.finish(test)
			want := eventMessagePublicMarker + " " + strings.Repeat("a🙂", 31000)
			if got := fixture.pages(test, generation, nil); got != want {
				test.Fatal("large single public block changed", len(got), len(want))
			}
			if fixture.steps["discovery"] < 2 || fixture.steps["message_inputs"] < 2 || fixture.steps["assembly"] < 2 || fixture.rawLoads["assembly"] != 0 || fixture.rawLoads["markdown"] != 0 {
				test.Fatal("large block did not exercise bounded multi-step orchestration", fixture.steps, fixture.rawLoads)
			}
			fixture.report(test, generation)
			fixture.latePage(test, generation, want)
		})
	}
}

func TestDocEventMessageManySmallPublicBlocksBoundedBatch(test *testing.T) {
	const count = 1200
	const maxTotalSteps = 1024
	blocks := make([]map[string]any, count)
	text := make([]string, count)
	for index := range blocks {
		text[index] = fmt.Sprintf("%s %04d 한글🙂", eventMessagePublicMarker, index)
		blocks[index] = map[string]any{"type": "text", "text": "**" + text[index] + "**"}
	}
	for _, summary := range []bool{false, true} {
		test.Run(fmt.Sprintf("summary=%t", summary), func(test *testing.T) {
			raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": summary, "blocks": blocks, "opaque": strings.Repeat(eventMessageOpaqueMarker, 3000)})
			if err != nil {
				test.Fatal(err)
			}
			fixture := newEventMessageFixture(test, raw)
			generation := fixture.finish(test)
			separator := "\n\n"
			if summary {
				separator = "\n"
			}
			want := strings.Join(text, separator)
			if got := fixture.pages(test, generation, nil); got != want {
				test.Fatal("many-block display changed", len(got), len(want))
			}
			totalSteps := fixture.report(test, generation)
			fixture.latePage(test, generation, want)
			if totalSteps > maxTotalSteps {
				test.Fatalf("bounded batching required: blocks=%d total_steps=%d maximum=%d; retain this requirement and optimize parent orchestration", count, totalSteps, maxTotalSteps)
			}
		})
	}
}

func TestDocEventMessageMultiPageUnicodeOriginalRuneSpans(test *testing.T) {
	for _, summary := range []bool{false, true} {
		raw := []byte(fmt.Sprintf(`{"kind":"message","compact_summary":%t,"blocks":[{"type":"text","text":"**MESSAGE_PUBLIC_BODY_MARKER %s**"},{"type":"image","name":"literal **한글🙂**"}]}`, summary, strings.Repeat(`\u0061\uD83D\uDE42`, 600)))
		fixture := newEventMessageFixture(test, raw)
		generation := fixture.finish(test)
		if generation.Pages.Count < 2 || fixture.steps["assembly"] < 2 {
			test.Fatal("Unicode fixture no longer crosses page boundaries", summary, generation.Pages.Count)
		}
		fixture.pages(test, generation, fixture.oracle(test))
	}
}

func TestDocEventMessageCheckpointValidationAndSealedGetters(test *testing.T) {
	fixture := newEventMessageFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"MESSAGE_PUBLIC_BODY_MARKER"}]}`))
	initial := fixture.state
	for _, mutate := range []func(*DocEventDisplayPreparation){
		func(candidate *DocEventDisplayPreparation) { candidate.Version = 99 },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.Version = 99 },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.Profile = "other-profile" },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.EventHash = "not-a-hash" },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.EventBytes = 0 },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.Kind = EventToolCall },
		func(candidate *DocEventDisplayPreparation) { candidate.Source.EmptyReasoning = true },
		func(candidate *DocEventDisplayPreparation) { candidate.Phase = "unknown" },
		func(candidate *DocEventDisplayPreparation) { candidate.Inputs = HashContent([]byte("inputs")) },
		func(candidate *DocEventDisplayPreparation) { candidate.Child = HashContent([]byte("child")) },
		func(candidate *DocEventDisplayPreparation) { candidate.Phase = "completed" },
	} {
		candidate := initial
		mutate(&candidate)
		if encoded, err := candidate.Encode(); !errors.Is(err, ErrIntegrity) || len(encoded) != 0 {
			test.Fatal("invalid checkpoint metadata encoded", candidate, err)
		}
		step, err := StepDocEventDisplay(context.Background(), candidate, fixture.loadRaw, fixture.stored)
		if !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid checkpoint metadata executed", err)
		}
		eventMessageZeroStep(test, step)
	}
	encoded, err := initial.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for _, invalid := range [][]byte{append(bytes.Clone(encoded), '\n'), []byte(`{"version":1}`), bytes.Replace(encoded, []byte(`"phase"`), []byte(`"unknown_field"`), 1)} {
		decoded, err := DecodeDocEventDisplayPreparation(HashContent(invalid), invalid)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocEventDisplayPreparation{}) {
			test.Fatal("noncanonical checkpoint decoded", err)
		}
	}
	step, err := StepDocEventDisplay(context.Background(), initial, fixture.loadRaw, fixture.stored)
	if err != nil || step.Preparation().Message == nil || len(step.Objects()) == 0 {
		test.Fatal("fixture did not produce message input metadata", err)
	}
	wantState, wantObjects, wantPrior := step.Preparation(), step.Objects(), step.Prior()
	candidate := step.Preparation()
	candidate.Message.Summary = !candidate.Message.Summary
	candidate.Message.Blocks.From++
	candidate.Message.EventHash = HashContent([]byte("mutated source"))
	objects := step.Objects()
	objects[0].Bytes[0] ^= 1
	objects[0].Hash = HashContent([]byte("mutated object"))
	if !reflect.DeepEqual(step.Preparation(), wantState) || !reflect.DeepEqual(step.Objects(), wantObjects) || step.Prior() != wantPrior {
		test.Fatal("sealed message preparation or object bytes alias getter results")
	}
}

func TestDocEventMessageBatchBudgetYieldAndLateFatalAreAtomic(test *testing.T) {
	blocks := []map[string]any{
		{"type": "text", "text": "**" + strings.Repeat("a", 40000) + "**"},
		{"type": "text", "text": "**" + strings.Repeat("b", 40000) + "**"},
	}
	raw, err := json.Marshal(map[string]any{"kind": "message", "blocks": blocks})
	if err != nil {
		test.Fatal(err)
	}
	fixture := newEventMessageFixture(test, raw)
	for fixture.state.Phase != "message_plan" {
		if err := fixture.advance(test); err != nil {
			test.Fatal(err)
		}
	}
	state := fixture.state
	before, err := state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	loads, rawReads, rawBytes := 0, 0, 0
	control, err := StepDocEventDisplay(context.Background(), state,
		func(_ context.Context, offset, length int) ([]byte, error) {
			loads++
			rawReads++
			rawBytes += length
			return bytes.Clone(raw[offset : offset+length]), nil
		},
		func(ctx context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.stored(ctx, hash) })
	if err != nil || control.Preparation().Phase != "message_plan" || rawBytes == 0 || rawBytes > 64<<10 || rawReads < 2 || len(control.Objects()) > 32 {
		test.Fatal("batch did not yield a bounded validated prefix", rawReads, rawBytes, len(control.Objects()), err)
	}
	for _, cancelLoad := range []bool{false, true} {
		failure := errors.New("fatal after successful batch transitions")
		ctx, cancel := context.WithCancel(context.Background())
		count := 0
		intercept := func(raw []byte) ([]byte, error) {
			count++
			if count == loads {
				if cancelLoad {
					cancel()
					return raw, nil
				}
				return nil, failure
			}
			return raw, nil
		}
		step, err := StepDocEventDisplay(ctx, state,
			func(_ context.Context, offset, length int) ([]byte, error) {
				return intercept(bytes.Clone(raw[offset : offset+length]))
			},
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				return intercept(bytes.Clone(fixture.objects[hash]))
			})
		cancel()
		want := failure
		if cancelLoad {
			want = context.Canceled
		}
		if !errors.Is(err, want) || count != loads {
			test.Fatal("late fatal error was converted into successful budget yield", count, loads, err)
		}
		eventMessageZeroStep(test, step)
		after, err := state.Encode()
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("late batch failure mutated predecessor", err)
		}
	}
	for _, object := range control.Objects() {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.state = fixture.restore(test, control.Preparation())
	partial, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.stored)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(partial, DocDisplayProjectionGeneration{}) {
		test.Fatal("budget prefix became partially publishable", err)
	}
	generation := fixture.finish(test)
	if got := fixture.pages(test, generation, nil); got != strings.Repeat("a", 40000)+"\n\n"+strings.Repeat("b", 40000) {
		test.Fatal("budget-yield restart duplicated or omitted a child", len(got))
	}
	test.Logf("budget_prefix_raw_loads=%d budget_prefix_raw_bytes=%d budget_prefix_object_loads=%d budget_prefix_staged_objects=%d", rawReads, rawBytes, loads-rawReads, len(control.Objects()))
}
