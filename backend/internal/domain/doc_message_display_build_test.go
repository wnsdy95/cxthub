package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type messageDisplayBuildFixture struct {
	source   DocMessageDisplaySource
	inputs   []DocMessageInput
	children []DocMessageDisplayBlockPlan
	objects  map[ContentHash][]byte
	raw      []byte
}

func buildMessageDisplayBuildFixture(test *testing.T, raw []byte) messageDisplayBuildFixture {
	test.Helper()
	message := buildMessageInputFixture(test, raw)
	fixture := messageDisplayBuildFixture{
		source:  DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: message.generation},
		inputs:  messageFixtureInputs(test, message),
		objects: message.objects,
		raw:     bytes.Clone(raw),
	}
	merge := func(objects map[ContentHash][]byte) {
		for hash, body := range objects {
			fixture.objects[hash] = bytes.Clone(body)
		}
	}
	for _, input := range fixture.inputs {
		var child DocMessageDisplayBlockPlan
		if input.IsText() {
			source, err := input.TextSource()
			if err != nil {
				test.Fatal(err)
			}
			if source != nil {
				markdown := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *source))
				child.Markdown = &markdown.plan
				merge(markdown.decoded.objects)
			}
		} else {
			blockType, name, err := input.PlaceholderSources()
			if err != nil {
				test.Fatal(err)
			}
			var prepared [2]*DocDecodedTextGeneration
			for index, source := range []*DocDecodedTextSource{blockType, name} {
				if source != nil {
					decoded := buildDecodedTextSourceFixture(test, raw, *source)
					prepared[index] = &decoded.generation
					merge(decoded.objects)
				}
			}
			placeholder, err := NewDocMessagePlaceholderPlan(input, prepared[0], prepared[1])
			if err != nil {
				test.Fatal(err)
			}
			child.Placeholder = &placeholder
		}
		fixture.children = append(fixture.children, child)
	}
	return fixture
}

func (fixture messageDisplayBuildFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	body, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(body), nil
}

func messageDisplayBuildEncode(test *testing.T, source DocMessageDisplaySource, state DocMessageDisplayState) []byte {
	test.Helper()
	raw, err := state.Encode(source)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		test.Fatal("invalid display checkpoint", len(raw), err)
	}
	return raw
}

func runMessageDisplayBuild(test *testing.T, fixture messageDisplayBuildFixture, batch int) (DocMessageDisplayState, DocMessageDisplayPlan, int) {
	test.Helper()
	state, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	partial := 0
	for !state.Done(fixture.source) {
		before := messageDisplayBuildEncode(test, fixture.source, state)
		count := min(batch, fixture.source.Input.Count-state.Next)
		loads := 0
		step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, state, fixture.children[state.Next:state.Next+count], func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return fixture.load(ctx, hash)
		})
		if err != nil || step.Processed < 1 || step.Processed > count || step.State.Next != state.Next+step.Processed || loads > DocDisplayMappedReplayObjects {
			test.Fatal("unbounded or stalled display step", state.Next, step.Processed, loads, err)
		}
		if step.Processed < count {
			partial++
			if loads != DocDisplayMappedReplayObjects {
				test.Fatal("yielded before the shared object budget", loads)
			}
		}
		if !bytes.Equal(before, messageDisplayBuildEncode(test, fixture.source, state)) {
			test.Fatal("prior display state mutated")
		}
		for _, object := range step.Objects {
			if HashContent(object.Bytes) != object.Hash || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocMessageDisplayPageBytes {
				test.Fatal("invalid display object", len(object.Bytes))
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded := messageDisplayBuildEncode(test, fixture.source, step.State)
		if len(step.State.Entries) >= DocMessageDisplayPageParts {
			test.Fatal("checkpoint retained a sealed page")
		}
		state, err = DecodeDocMessageDisplayState(fixture.source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("cannot resume display state", err)
		}
		if err := ValidateDocMessageDisplayCheckpoint(context.Background(), fixture.source, state, fixture.load); err != nil {
			test.Fatal("invalid display checkpoint proof", err)
		}
	}
	plan, objects, err := FinishDocMessageDisplayPlan(context.Background(), fixture.source, state, fixture.load)
	if err != nil || plan.Count != state.Count || plan.TextBytes != state.TextBytes {
		test.Fatal("cannot complete display plan", err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	return state, plan, partial
}

func TestDocMessageDisplayBuildOriginalSourcesAndSeparators(test *testing.T) {
	raw := []byte(`{"kind":"message","opaque":{"text":"PRIVATE"},"blocks":[{"type":"text","text":null},{"type":"text","text":"\u0060\u0060\u0060\n\u0060\u0060\u0060"},{"type":"text","text":"# \uD83D\uDE42 **title**"},{"type":"text"},{"type":"image","name":"**literal**","text":"PRIVATE"},{"type":"text","text":"[label](https://hidden.test)"},{"type":"text","text":""}]}`)
	fixture := buildMessageDisplayBuildFixture(test, raw)
	state, plan, _ := runMessageDisplayBuild(test, fixture, 1)
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil || state.Count != 3 || plan.TextBytes != len(baseline.Text()) {
		test.Fatal("message display totals changed", state.Count, plan.TextBytes, err)
	}
	parts, err := ReadDocMessageDisplayParts(context.Background(), plan, 0, fixture.load)
	if err != nil || len(parts) != 3 {
		test.Fatal(err)
	}
	for index, part := range parts {
		if part.Block != []int{2, 4, 5}[index] || part.Separator != []int{0, 2, 2}[index] {
			test.Fatal("visible ordinal or separator changed", part)
		}
		text := baseline.Text()[part.From+part.Separator : part.end()]
		_, width := utf8.DecodeRuneInString(text)
		first, err := baseline.SourceRange(part.From+part.Separator, width)
		if err != nil || first != part.First {
			test.Fatal("first original rune mapping changed", part.First, first, err)
		}
		if part.Separator > 0 {
			anchor, err := baseline.SourceRange(part.From, part.Separator)
			if err != nil || anchor != part.First {
				test.Fatal("separator lost its first-rune anchor", anchor, err)
			}
		}
		bound, err := bindDocMessageDisplayChild(fixture.inputs[part.Block], fixture.children[part.Block])
		if err != nil || part.Kind != bound.Kind || part.Plan != HashContent(bound.Raw) || part.TextBytes != bound.Input.source.TextBytes || !bytes.Equal(fixture.objects[part.Plan], bound.Raw) {
			test.Fatal("child object not preserved", part, err)
		}
	}
	if string(raw[parts[0].First.From:parts[0].First.To]) != `\uD83D\uDE42` || parts[1].First != fixture.inputs[4].Anchor() {
		test.Fatal("source coordinates are not original JSON coordinates", parts)
	}
	for _, block := range []int{1, 6} {
		raw, err := fixture.children[block].Markdown.Encode()
		if err != nil || fixture.objects[HashContent(raw)] != nil {
			test.Fatal("empty child plan was persisted", block, err)
		}
	}
}

func TestDocMessageDisplayBuildEmptyMarkdownCarriesBreaks(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"left"},{"type":"text","text":"\u0060\u0060\u0060\n\u0060\u0060\u0060"},{"type":"text"},{"type":"text","text":null},{"type":"text","text":""},{"type":"text","text":"right"}]}`))
	state, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	for index, child := range fixture.children {
		step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, state, []DocMessageDisplayBlockPlan{child}, fixture.load)
		if err != nil || step.Processed != 1 {
			test.Fatal(index, err)
		}
		state = step.State
		for _, object := range step.Objects {
			fixture.objects[object.Hash] = object.Bytes
		}
		if index > 0 && index < 5 && (state.Breaks != 2 || state.Count != 1 || state.TextBytes != 4 || len(step.Objects) != 0) {
			test.Fatal("empty or missing text changed pending breaks", index, state)
		}
	}
	plan, objects, err := FinishDocMessageDisplayPlan(context.Background(), fixture.source, state, fixture.load)
	if err != nil || plan.TextBytes != len("left\n\nright") {
		test.Fatal("empty Markdown lost block separation", plan, err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	parts, err := ReadDocMessageDisplayParts(context.Background(), plan, 0, fixture.load)
	if err != nil || parts[1].Separator != 2 || parts[1].From != 4 || parts[1].Block != 5 {
		test.Fatal(parts, err)
	}
}

func TestDocMessageDisplayBuildSharedBinderRejectsSubstitution(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"same"},{"type":"text","text":"same"},{"type":"text"},{"type":"text","text":null},{"type":"text","text":""},{"type":"image","name":"asset"},{"type":"image","name":"asset"}]}`))
	foreign := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"foreign"},{"type":"image","name":"asset"}]}`))
	for _, scenario := range []struct {
		name  string
		input int
		child DocMessageDisplayBlockPlan
	}{
		{"wrong-token", 0, fixture.children[1]},
		{"wrong-event", 0, foreign.children[0]},
		{"missing-text-plan", 0, DocMessageDisplayBlockPlan{}},
		{"empty-text-needs-plan", 4, DocMessageDisplayBlockPlan{}},
		{"missing-text-has-plan", 2, fixture.children[0]},
		{"null-text-has-plan", 3, fixture.children[0]},
		{"placeholder-as-text", 0, fixture.children[5]},
		{"text-as-placeholder", 5, fixture.children[0]},
		{"missing-placeholder", 5, DocMessageDisplayBlockPlan{}},
		{"wrong-block", 5, fixture.children[6]},
		{"wrong-placeholder-source", 5, foreign.children[1]},
		{"two-children", 0, DocMessageDisplayBlockPlan{Markdown: fixture.children[0].Markdown, Placeholder: fixture.children[5].Placeholder}},
		{"two-placeholder-children", 5, DocMessageDisplayBlockPlan{Markdown: fixture.children[0].Markdown, Placeholder: fixture.children[5].Placeholder}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			bound, err := bindDocMessageDisplayChild(fixture.inputs[scenario.input], scenario.child)
			if !errors.Is(err, ErrIntegrity) || bound.Kind != "" || bound.Raw != nil || bound.Input.read != nil {
				test.Fatal("substituted child accepted", err)
			}
		})
	}
	for _, index := range []int{2, 3} {
		bound, err := bindDocMessageDisplayChild(fixture.inputs[index], fixture.children[index])
		if err != nil || bound.Kind != "" || bound.Input.read != nil {
			test.Fatal("missing or null text became a child", index, err)
		}
	}
	bound, err := bindDocMessageDisplayChild(fixture.inputs[4], fixture.children[4])
	if err != nil || bound.Kind != DocMessageDisplayMarkdown || bound.Input.validate() != nil || bound.Input.source.TextBytes != 0 || bound.Raw != nil {
		test.Fatal("empty Markdown lost its typed input", err)
	}
	raw, err := fixture.children[5].Placeholder.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocMessagePlaceholderPlan(HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	bound, err = bindDocMessageDisplayChild(fixture.inputs[5], DocMessageDisplayBlockPlan{Placeholder: &decoded})
	if err != nil || bound.Leading != 2 || bound.Trailing != 2 || !bytes.Equal(bound.Raw, raw) {
		test.Fatal("independently decoded placeholder did not bind by value", err)
	}
}

func TestDocMessageDisplayBuildPagedResumeAndFrontier(test *testing.T) {
	count := DocMessageDisplayPageParts*DocRangeDirectoryFanout + 3
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat("{},", count-1)+`{}]}`))
	state, plan, partial := runMessageDisplayBuild(test, fixture, DocMessageDisplayStepBlocks)
	if state.Next != count || state.Count != count || len(state.Entries) != 0 || plan.Parts.Count != DocRangeDirectoryFanout+1 || len(state.Directory.Levels) != 2 || partial != 0 || plan.TextBytes != count*4-2 {
		test.Fatal("paged display totals or frontier changed", state.Next, state.Count, plan.Parts, state.Directory, partial)
	}
	previous := 0
	for index := 0; index < plan.Parts.Count; index++ {
		parts, err := ReadDocMessageDisplayParts(context.Background(), plan, index, fixture.load)
		if err != nil || len(parts) != min(DocMessageDisplayPageParts, count-index*DocMessageDisplayPageParts) {
			test.Fatal("cannot read display part page", index, err)
		}
		for _, part := range parts {
			if part.Index != part.Block || part.From != previous || part.First != fixture.inputs[part.Block].Anchor() || part.Kind != DocMessageDisplayPlaceholder {
				test.Fatal("display page order changed", part)
			}
			previous = part.end()
		}
	}
	if previous != plan.TextBytes {
		test.Fatal("display pages do not cover the output")
	}
}

func TestDocMessageDisplayBuildSharedObjectBudget(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat(`{"type":"text","text":"\uD83D\uDE42 **body**"},`, 63)+`{"type":"text","text":"last"}]}`))
	state, _, partial := runMessageDisplayBuild(test, fixture, DocMessageDisplayStepBlocks)
	if partial == 0 || state.Next != 64 || state.Count != 64 {
		test.Fatal("the step did not share one actual-load budget", partial, state.Next)
	}
	initial, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:1], func(context.Context, ContentHash) ([]byte, error) {
		return nil, errDocMappedReplayBudget
	})
	if !errors.Is(err, errDocMappedReplayBudget) || !reflect.DeepEqual(step, DocMessageDisplayStep{}) {
		test.Fatal("zero-progress budget failure returned effects", step, err)
	}
}

func TestDocMessageDisplayBuildFailuresAreAtomic(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"first"},{"type":"text","text":"second"},{"type":"text","text":"third"}]}`))
	initial, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	first, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:1], fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	prior := first.State
	prior.Entries = make([]DocMessageDisplayPart, len(first.State.Entries), DocMessageDisplayPageParts)
	copy(prior.Entries, first.State.Entries)
	before := messageDisplayBuildEncode(test, fixture.source, prior)
	backing := append([]DocMessageDisplayPart(nil), prior.Entries[:cap(prior.Entries)]...)
	selected := fixture.children[2].Markdown.Input.Pages.Hash
	for _, scenario := range []struct {
		name string
		want error
	}{
		{"missing", ErrNotFound},
		{"corrupt", ErrIntegrity},
		{"cancel-during-load", context.Canceled},
		{"cancel-before-load", context.Canceled},
		{"late-child-mismatch", ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario.name == "cancel-before-load" {
				cancel()
			}
			children := append([]DocMessageDisplayBlockPlan(nil), fixture.children[1:]...)
			if scenario.name == "late-child-mismatch" {
				children[1] = fixture.children[0]
			}
			loads := 0
			step, err := StepDocMessageDisplayPlan(ctx, fixture.source, prior, children, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				loads++
				if hash == selected {
					switch scenario.name {
					case "missing":
						return nil, ErrNotFound
					case "corrupt":
						return []byte("corrupt"), nil
					case "cancel-during-load":
						cancel()
					}
				}
				return fixture.load(ctx, hash)
			})
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(step, DocMessageDisplayStep{}) || scenario.name == "cancel-before-load" && loads != 0 {
				test.Fatal("failed step returned partial effects", step, loads, err)
			}
			if !bytes.Equal(before, messageDisplayBuildEncode(test, fixture.source, prior)) || !reflect.DeepEqual(backing, prior.Entries[:cap(prior.Entries)]) {
				test.Fatal("failed step mutated prior state or its backing array")
			}
		})
	}
}

func TestDocMessageDisplayBuildCheckpointBeforeSourceAndFinish(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat("{},", 129)+`{}]}`))
	initial, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	first, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:64], fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range first.Objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	second, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, first.State, fixture.children[64:65], fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	forged := second.State
	forged.Entries = append([]DocMessageDisplayPart(nil), forged.Entries...)
	forged.Entries[0].From++
	forged.TextBytes++
	if forged.Validate(fixture.source) != nil {
		test.Fatal("fixture did not require a checkpoint proof")
	}
	sourceReads := 0
	step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, forged, fixture.children[65:66], func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if hash == fixture.source.Input.Pages.Hash {
			sourceReads++
		}
		return fixture.load(ctx, hash)
	})
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocMessageDisplayStep{}) || sourceReads != 0 {
		test.Fatal("source was read before checkpoint validation", sourceReads, err)
	}
	state, _, _ := runMessageDisplayBuild(test, fixture, 64)
	state.TextBytes++
	if state.Validate(fixture.source) != nil {
		test.Fatal("completion proof fixture is structurally invalid")
	}
	plan, objects, err := FinishDocMessageDisplayPlan(context.Background(), fixture.source, state, fixture.load)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(plan, DocMessageDisplayPlan{}) || len(objects) != 0 {
		test.Fatal("completion accepted an unproved byte total", err)
	}
	state.TextBytes--
	state.Next--
	if _, _, err := FinishDocMessageDisplayPlan(context.Background(), fixture.source, state, fixture.load); !errors.Is(err, ErrIntegrity) {
		test.Fatal("completion accepted an unfinished original-block cursor", err)
	}
}

func TestDocMessageDisplayBuildStateAndBatchLimits(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat("{},", 65)+`{}]}`))
	initial, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	for _, children := range [][]DocMessageDisplayBlockPlan{nil, fixture.children[:65], fixture.children} {
		loads := 0
		step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, children, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return fixture.load(ctx, hash)
		})
		if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(step, DocMessageDisplayStep{}) {
			test.Fatal("invalid batch performed work", len(children), loads, err)
		}
	}
	step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:1], fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*DocMessageDisplayState){
		func(state *DocMessageDisplayState) { state.Version++ },
		func(state *DocMessageDisplayState) { state.Source = HashContent([]byte("other")) },
		func(state *DocMessageDisplayState) { state.Next = -1 },
		func(state *DocMessageDisplayState) { state.Next = fixture.source.Input.Count + 1 },
		func(state *DocMessageDisplayState) { state.Next = 0 },
		func(state *DocMessageDisplayState) { state.Count = 2 },
		func(state *DocMessageDisplayState) { state.TextBytes++ },
		func(state *DocMessageDisplayState) { state.Breaks = -1 },
		func(state *DocMessageDisplayState) { state.Breaks = 3 },
		func(state *DocMessageDisplayState) { state.Breaks = 0 },
		func(state *DocMessageDisplayState) { state.Entries = make([]DocMessageDisplayPart, 64) },
		func(state *DocMessageDisplayState) { state.Entries[0].Block = state.Next },
		func(state *DocMessageDisplayState) { state.Entries[0].Index++ },
		func(state *DocMessageDisplayState) { state.Entries[0].Separator = 1 },
		func(state *DocMessageDisplayState) { state.Directory.Count++ },
	} {
		forged := step.State
		forged.Entries = append([]DocMessageDisplayPart(nil), step.State.Entries...)
		mutate(&forged)
		if forged.Validate(fixture.source) == nil {
			test.Fatal("invalid display checkpoint accepted", forged)
		}
	}
	encoded := messageDisplayBuildEncode(test, fixture.source, step.State)
	unknown := append(bytes.Clone(encoded[:len(encoded)-1]), []byte(`,"unknown":true}`)...)
	for _, raw := range [][]byte{nil, append(bytes.Clone(encoded), ' '), unknown, bytes.Repeat([]byte("x"), MaxDocDisplayStateBytes+1)} {
		if _, err := DecodeDocMessageDisplayState(fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical or oversized display state accepted", len(raw), err)
		}
	}
	if _, err := DecodeDocMessageDisplayState(fixture.source, HashContent([]byte("wrong")), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("display checkpoint hash mismatch accepted", err)
	}
	changed := fixture.source
	changed.Input.Source.EventHash = HashContent([]byte("other"))
	if _, err := DecodeDocMessageDisplayState(changed, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("display checkpoint source mismatch accepted", err)
	}
	if _, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:1], nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil loader accepted", err)
	}
}

func TestDocMessageDisplayBuildDisplayedByteCap(test *testing.T) {
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{},{}]}`))
	initial, err := NewDocMessageDisplayState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	first, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, initial, fixture.children[:1], fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name      string
		remaining int
		invalid   bool
		want      error
	}{
		{"body-exceeds-cap", 1, false, ErrDocFindDisplayUnavailable},
		{"separator-exceeds-cap", 2, false, ErrDocFindDisplayUnavailable},
		{"combined-exceeds-cap", 3, false, ErrDocFindDisplayUnavailable},
		{"exact-boundary", 4, false, nil},
		{"invalid-child", 3, true, ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			prior := first.State
			prior.Entries = append([]DocMessageDisplayPart(nil), first.State.Entries...)
			prior.TextBytes = MaxConversationDocumentBytes - scenario.remaining
			prior.Entries[0].TextBytes = prior.TextBytes
			before := messageDisplayBuildEncode(test, fixture.source, prior)
			child := fixture.children[1]
			if scenario.invalid {
				placeholder := *child.Placeholder
				placeholder.TextBytes++
				child.Placeholder = &placeholder
			}
			step, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, prior, []DocMessageDisplayBlockPlan{child}, fixture.load)
			if !errors.Is(err, scenario.want) {
				test.Fatal("wrong displayed-byte-cap error", err, scenario.want)
			}
			if scenario.want != nil && !reflect.DeepEqual(step, DocMessageDisplayStep{}) {
				test.Fatal("rejected display returned partial effects", step)
			}
			if scenario.want == nil && (step.State.TextBytes != MaxConversationDocumentBytes || step.Processed != 1 || !step.State.Done(fixture.source)) {
				test.Fatal("exact displayed-byte boundary rejected", step.State)
			}
			if !bytes.Equal(before, messageDisplayBuildEncode(test, fixture.source, prior)) {
				test.Fatal("displayed-byte-cap check mutated prior state")
			}
		})
	}
}

func TestDocMessageDisplayBuildEmptyAndCompletionErrors(test *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"kind":"message"}`),
		[]byte(`{"kind":"message","blocks":[]}`),
		[]byte(`{"kind":"message","blocks":[{"type":"text"},{"type":"text","text":null},{"type":"text","text":"\u0060\u0060\u0060\n\u0060\u0060\u0060"}]}`),
	} {
		fixture := buildMessageDisplayBuildFixture(test, raw)
		state, plan, _ := runMessageDisplayBuild(test, fixture, 64)
		if !state.Done(fixture.source) || state.Count != 0 || plan.Count != 0 || plan.TextBytes != 0 || plan.Parts.Count != 0 {
			test.Fatal("empty message generated display objects", state, plan)
		}
		if _, err := StepDocMessageDisplayPlan(context.Background(), fixture.source, state, nil, fixture.load); !errors.Is(err, ErrIntegrity) {
			test.Fatal("completed builder accepted another step", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, objects, err := FinishDocMessageDisplayPlan(ctx, fixture.source, state, fixture.load); !errors.Is(err, context.Canceled) || len(objects) != 0 {
			test.Fatal("canceled completion returned effects", err)
		}
	}
	fixture := buildMessageDisplayBuildFixture(test, []byte(`{"kind":"message","blocks":[{}]}`))
	state, _, _ := runMessageDisplayBuild(test, fixture, 1)
	if _, _, err := FinishDocMessageDisplayPlan(context.Background(), fixture.source, state, nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("completion skipped the sealed-page proof", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plan, objects, err := FinishDocMessageDisplayPlan(ctx, fixture.source, state, func(context.Context, ContentHash) ([]byte, error) {
		cancel()
		return nil, ErrNotFound
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(plan, DocMessageDisplayPlan{}) || len(objects) != 0 {
		test.Fatal("cancellation did not override completion failure", err)
	}
}
