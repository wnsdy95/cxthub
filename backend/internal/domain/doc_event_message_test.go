package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
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

func TestDocEventMessageLegacyReasoningCanonicalCompatibility(test *testing.T) {
	raw := []byte("{\"kind\":\"reasoning\",\"redacted_summary\":\"legacy **\ud55c\uae00🙂**\"}")
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

func eventMessageRestartBody(summary bool) []byte {
	return []byte(fmt.Sprintf("{\"kind\":\"message\",\"compact_summary\":%t,\"opaque\":{\"blocks\":\"%s\"},\"blocks\":[{\"type\":\"text\",\"text\":\"# **MESSAGE_PUBLIC_BODY_MARKER \ud55c\uae00\\uD83D\\uDE42**\"},{\"type\":\"**image**\",\"name\":\"[MESSAGE_PUBLIC_BODY_MARKER](literal)\"},{\"type\":\"text\",\"text\":\"tail\"}]}", summary, strings.Repeat(eventMessageOpaqueMarker, 3000)))
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
