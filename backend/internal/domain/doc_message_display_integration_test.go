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

type messageDisplayFixture struct {
	plan          DocMessageDisplayPlan
	objects       map[ContentHash][]byte
	steps         int
	maxCheckpoint int
	maxLoads      int
}

func messageDisplayChildren(test *testing.T, raw []byte, inputs []DocMessageInput, objects map[ContentHash][]byte) []DocMessageDisplayBlockPlan {
	test.Helper()
	children := make([]DocMessageDisplayBlockPlan, len(inputs))
	merge := func(prepared map[ContentHash][]byte) {
		for hash, body := range prepared {
			objects[hash] = bytes.Clone(body)
		}
	}
	for index, input := range inputs {
		if input.IsText() {
			source, err := input.TextSource()
			if err != nil {
				test.Fatal(err)
			}
			if source == nil {
				continue
			}
			prepared := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *source))
			children[index].Markdown = &prepared.plan
			merge(prepared.decoded.objects)
			continue
		}
		blockType, name, err := input.PlaceholderSources()
		if err != nil {
			test.Fatal(err)
		}
		var decoded [2]*DocDecodedTextGeneration
		for position, source := range []*DocDecodedTextSource{blockType, name} {
			if source == nil {
				continue
			}
			prepared := buildDecodedTextSourceFixture(test, raw, *source)
			decoded[position] = &prepared.generation
			merge(prepared.objects)
		}
		plan, err := NewDocMessagePlaceholderPlan(input, decoded[0], decoded[1])
		if err != nil {
			test.Fatal(err)
		}
		children[index].Placeholder = &plan
	}
	return children
}

func buildMessageDisplayFixture(test *testing.T, raw []byte) messageDisplayFixture {
	test.Helper()
	message := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, message)
	children := messageDisplayChildren(test, raw, inputs, message.objects)
	source := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: message.generation}
	state, err := NewDocMessageDisplayState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := messageDisplayFixture{objects: message.objects}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Done(source) {
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		loads := 0
		step, err := StepDocMessageDisplayPlan(context.Background(), source, state, children[state.Next:min(state.Next+DocMessageDisplayStepBlocks, len(children))], func(ctx context.Context, hash ContentHash) ([]byte, error) { loads++; return load(ctx, hash) })
		if err != nil || step.Processed <= 0 || step.Processed > DocMessageDisplayStepBlocks || step.State.Next != state.Next+step.Processed || loads > DocDisplayMappedReplayObjects {
			test.Fatal("message display plan failed bounded progress", state.Next, step.Processed, loads, err)
		}
		fixture.steps++
		fixture.maxLoads = max(fixture.maxLoads, loads)
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("message plan mutated previous checkpoint", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) > MaxDocMessageDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid message-plan object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(source)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("invalid or body-retaining message plan checkpoint", err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		state, err = DecodeDocMessageDisplayState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
		if err := ValidateDocMessageDisplayCheckpoint(context.Background(), source, state, load); err != nil {
			test.Fatal("message checkpoint failed restart", err)
		}
	}
	plan, produced, err := FinishDocMessageDisplayPlan(context.Background(), source, state, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range produced {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.plan, err = DecodeDocMessageDisplayPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestDocMessageDisplayMixedBlocksMatchWholeEventOracle(test *testing.T) {
	for _, blocks := range []string{
		`[]`,
		`[{"type":"text"},{"type":"text","text":null},{"type":"text","text":""}]`,
		`[{"type":"text","text":"**first"},{"type":"text","text":"last**"}]`,
		`[{"type":"text","text":"# **한글\uD83D\uDE42**"},{"type":"image","name":"**asset**"},{"type":"text","text":"- end"}]`,
		`[{"type":"text","text":"# first"},{"type":"text","text":""},{"type":"text","text":"\n\n# "},{"type":"text","text":"# last"}]`,
		`[{"type":"text","text":"paragraph"},{"type":"text","text":"# "},{"type":"text","text":null},{"type":"text","text":"- next"}]`,
		`[{},{"type":null,"name":"asset"},{"type":"Text","text":"not a text block"}]`,
		"[{\"type\":\"text\",\"text\":\"```\\n**literal**\\n```\"},{\"type\":\"text\",\"text\":\"**last**\"}]",
	} {
		test.Run(blocks, func(test *testing.T) {
			raw := []byte(`{"kind":"message","blocks":` + blocks + `}`)
			fixture := buildMessageDisplayFixture(test, raw)
			input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
			if err != nil {
				test.Fatal(err)
			}
			baseline, err := BuildDocEventDisplay(context.Background(), raw)
			if err != nil {
				test.Fatal(err)
			}
			assertMappedAssemblyReplay(test, input, fixture.objects, baseline)
			assembled := assembleDisplayFixture(test, input, fixture.objects)
			output := assembledFixturePages(test, assembled, func(page DocDisplayPage) {
				for offset, character := range page.Text {
					got, err := page.SourceRange(offset, utf8.RuneLen(character))
					want, failure := baseline.SourceRange(page.From+offset, utf8.RuneLen(character))
					if err != nil || failure != nil || got != want {
						test.Fatal("composed message source changed", got, want, err, failure)
					}
				}
			})
			if output != baseline.Text() {
				test.Fatal("ordinary message boundaries changed", output, baseline.Text())
			}
		})
	}
}

func TestDocMessageDisplayLargePagedSequenceAndLatePart(test *testing.T) {
	const count = 1200
	var raw strings.Builder
	raw.WriteString(`{"kind":"message","blocks":[`)
	var want strings.Builder
	for index := 0; index < count; index++ {
		if index > 0 {
			raw.WriteByte(',')
			want.WriteString("\n\n")
		}
		fmt.Fprintf(&raw, `{"type":"text","text":"**block %d**","opaque":"private"}`, index)
		fmt.Fprintf(&want, "block %d", index)
	}
	raw.WriteString(`]}`)
	fixture := buildMessageDisplayFixture(test, []byte(raw.String()))
	if fixture.plan.Count != count || fixture.plan.Parts.Count != (count+DocMessageDisplayPageParts-1)/DocMessageDisplayPageParts {
		test.Fatal("large message lost parts", fixture.plan.Count, fixture.plan.Parts.Count)
	}
	loads := 0
	last, err := ReadDocMessageDisplayParts(context.Background(), fixture.plan, fixture.plan.Parts.Count-1, func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
	if err != nil || len(last) == 0 || last[len(last)-1].Block != count-1 || loads > 4 {
		test.Fatal("late part scanned preceding plan pages", loads, err)
	}
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	assembled := assembleDisplayFixture(test, input, fixture.objects)
	if got := assembledFixturePages(test, assembled, nil); got != want.String() {
		test.Fatal("large ordinary message changed", len(got), want.Len())
	}
	test.Logf("event_bytes=%d blocks=%d plan_pages=%d build_steps=%d max_build_loads=%d checkpoint_bytes=%d late_part_loads=%d display_bytes=%d assembly_steps=%d display_pages=%d", raw.Len(), count, fixture.plan.Parts.Count, fixture.steps, fixture.maxLoads, fixture.maxCheckpoint, loads, assembled.generation.Source.TextBytes, assembled.steps, assembled.generation.Pages.Count)
}

func TestDocMessageDisplayBatchReadBudgetAndAtomicFailure(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{"type":"text","text":"**a**"},`, 39) + `{"type":"text","text":"**a**"}]}`)
	fixture := buildMessageDisplayFixture(test, raw)
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	before := bytes.Clone(input.initial)
	loads := 0
	page, next, err := input.read(context.Background(), input.initial, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return fixture.objects[hash], nil
	})
	if err != nil || loads != DocDisplayMappedReplayObjects || len(page.Text) <= 1 || next == nil || !bytes.Equal(before, input.initial) {
		test.Fatal("message batch did not yield bounded multi-part progress", page.Text, loads, err)
	}
	for _, cancelRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		failed, cursor, err := input.read(ctx, input.initial, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
			calls++
			if calls == 30 {
				if cancelRead {
					cancel()
				} else {
					return nil, ErrNotFound
				}
			}
			return fixture.objects[hash], nil
		})
		cancel()
		want := ErrNotFound
		if cancelRead {
			want = context.Canceled
		}
		if !errors.Is(err, want) || !reflect.DeepEqual(failed, DocDisplayPage{}) || cursor != nil || !bytes.Equal(before, input.initial) {
			test.Fatal("partially produced batch survived a fatal error", cancelRead, failed.Text, err)
		}
	}
}

func FuzzDocMessageDisplayWholeEventParity(fuzz *testing.F) {
	for _, text := range []string{"", "**first", "# 한글🙂", "a\n\nb", "# ", "\u0000\r\t"} {
		fuzz.Add(text, "last**", true)
		fuzz.Add(text, "asset", false)
	}
	fuzz.Fuzz(func(test *testing.T, first, last string, textBlock bool) {
		if len(first)+len(last) > 1024 || !utf8.ValidString(first) || !utf8.ValidString(last) || strings.HasPrefix(strings.TrimLeftFunc(first, docRangePresentationSpace), "[cxt") {
			test.Skip()
		}
		second := map[string]any{"type": "image", "name": last, "text": "hidden"}
		if textBlock {
			second = map[string]any{"type": "text", "text": last}
		}
		raw, err := json.Marshal(map[string]any{"kind": "message", "blocks": []any{map[string]any{"type": "text", "text": first}, map[string]any{"type": "text", "text": "# "}, second}})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildMessageDisplayFixture(test, raw)
		input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		assertMappedAssemblyReplay(test, input, fixture.objects, baseline)
	})
}
