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

type messageInputFixture struct {
	source        DocMessageInputSource
	state         DocMessageInputState
	generation    DocMessageInputGeneration
	objects       map[ContentHash][]byte
	steps         int
	maxCheckpoint int
	maxReads      int
	maxEntries    int
}

func buildMessageInputFixture(test *testing.T, raw []byte) messageInputFixture {
	test.Helper()
	discovery, err := discoverDocFieldReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal("source discovery", err)
	}
	entry, err := docRangeEntry(0, 0, raw, 0)
	if err != nil {
		test.Fatal("range metadata", err)
	}
	source, err := discovery.MessageInputSource(entry)
	if err != nil {
		test.Fatal("message input source", err)
	}
	state, err := NewDocMessageInputState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := messageInputFixture{source: source, objects: make(map[ContentHash][]byte)}
	for !state.Done() {
		fixture.steps++
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal("invalid checkpoint", state.Phase, state.Next, err)
		}
		reads := 0
		step, err := StepDocMessageInput(context.Background(), source, state,
			func(_ context.Context, from, length int) ([]byte, error) {
				if from <= source.Blocks.From || length <= 0 || length > 4096 || from > source.Blocks.To-length {
					test.Fatal("source overread", from, length, source.Blocks)
				}
				reads += length
				return raw[from : from+length], nil
			},
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				body, found := fixture.objects[hash]
				if !found {
					return nil, ErrNotFound
				}
				return body, nil
			})
		if err != nil {
			test.Fatal("message scan step", fixture.steps, state.Phase, state.Next, state.Count, err)
		}
		if reads != step.ReadBytes || reads > DocDisplayStepSourceBytes || step.State.Count-state.Count > DocMessageInputStepEntries {
			test.Fatal("scan budget exceeded", reads, step.ReadBytes, step.State.Count-state.Count)
		}
		fixture.maxReads, fixture.maxEntries = max(fixture.maxReads, reads), max(fixture.maxEntries, step.State.Count-state.Count)
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("checkpoint mutated", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) == 0 || len(object.Bytes) > max(MaxDocMessageInputPageBytes, MaxDocRangeDirectoryBytes) || HashContent(object.Bytes) != object.Hash {
				test.Fatal("bad staged object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(source)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) {
			test.Fatal("invalid or stalled checkpoint", len(encoded), err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		state, err = DecodeDocMessageInputState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("cannot resume message input", err)
		}
	}
	generation, objects, err := FinishDocMessageInput(context.Background(), source, state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.state, fixture.generation = state, generation
	return fixture
}

func messageFixtureInputs(test *testing.T, fixture messageInputFixture) []DocMessageInput {
	test.Helper()
	var inputs []DocMessageInput
	for index := 0; index < fixture.generation.Pages.Count; index++ {
		page, err := ReadDocMessageInputs(context.Background(), fixture.generation, index,
			func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
		if err != nil {
			test.Fatal(err)
		}
		inputs = append(inputs, page...)
	}
	if len(inputs) != fixture.generation.Count {
		test.Fatal("entry count changed", len(inputs), fixture.generation.Count)
	}
	return inputs
}

func messageFixtureString(test *testing.T, raw []byte, source *DocDecodedTextSource) string {
	test.Helper()
	if source == nil {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw[source.Token.From:source.Token.To], &text); err != nil {
		test.Fatal(err)
	}
	return text
}

func TestDocMessageInputExactRootAndBlockSelection(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"discard"}],"opaque":{"blocks":[{"type":"text","text":"locked"}]},"blo\u0063ks":[{"type":"text","text":42,"text":"**safe**","opaque":{"type":"text","text":"locked","name":"hidden"}},{"type":"image","name":"old","na\u006de":"asset","text":"not a normal text block"},{"TYPE":"text","text":"case is not text"}]}`)
	fixture := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, fixture)
	if len(inputs) != 3 || fixture.source.Summary {
		test.Fatal(len(inputs), fixture.source)
	}
	text, err := inputs[0].TextSource()
	if err != nil || messageFixtureString(test, raw, text) != "**safe**" || inputs[0].Index() != 0 {
		test.Fatal("final direct text not selected", text, err)
	}
	blockType, name, err := inputs[1].PlaceholderSources()
	if err != nil || messageFixtureString(test, raw, blockType) != "image" || messageFixtureString(test, raw, name) != "asset" {
		test.Fatal("placeholder input wrong", blockType, name, err)
	}
	if _, err := inputs[1].TextSource(); !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal("nontext block exposed text as normal body", err)
	}
	blockType, name, err = inputs[2].PlaceholderSources()
	if err != nil || blockType != nil || name != nil {
		test.Fatal("case variant became an exact type key", blockType, name, err)
	}
}

func TestDocMessageInputMissingNullEmptyAndSummarySlots(test *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"kind":"message"}`),
		[]byte(`{"kind":"message","blocks":null}`),
		[]byte(`{"kind":"message","blocks":[]}`),
		[]byte(`{"kind":"message","blocks":[{"type":"text","text":"discard"}],"blocks":null}`),
	} {
		fixture := buildMessageInputFixture(test, raw)
		if fixture.generation.Count != 0 || fixture.generation.Pages.Count != 0 {
			test.Fatal("empty message retained prior blocks", fixture.generation)
		}
	}
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"type":"text","text":"**first"},{"type":"image","name":42,"text":null},{"type":null,"text":"last**"},{}]}`)
	fixture := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, fixture)
	var slots []string
	for _, input := range inputs {
		if !input.SummarySlot() {
			test.Fatal("lost summary mode")
		}
		source, err := input.TextSource()
		if err != nil {
			test.Fatal(err)
		}
		slots = append(slots, messageFixtureString(test, raw, source))
	}
	if strings.Join(slots, "\n") != "**first\n\nlast**\n" {
		test.Fatal("summary slots were filtered", slots)
	}
	for _, pair := range []struct{ name, blocks string }{
		{"type", `[{"type":"text","type":null,"text":"ignored"}]`},
		{"text", `[{"type":"text","text":"old","text":null}]`},
	} {
		itemRaw := []byte(`{"kind":"message","blocks":` + pair.blocks + `}`)
		item := messageFixtureInputs(test, buildMessageInputFixture(test, itemRaw))[0]
		if pair.name == "type" && item.IsText() {
			test.Fatal("null did not clear prior type")
		}
		if pair.name == "text" {
			source, err := item.TextSource()
			if err != nil || source != nil {
				test.Fatal("null did not clear prior text", source, err)
			}
		}
	}
}

func TestDocMessageInputLargeOpaqueAndSelectedPages(test *testing.T) {
	const count = 9000
	var raw strings.Builder
	raw.WriteString(`{"kind":"message","blocks":[`)
	for index := 0; index < count; index++ {
		if index > 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"type":"text","text":"block %d","opaque":{"text":"`, index)
		if index == 67 {
			raw.WriteString(strings.Repeat("PRIVATE_LOCKED_BODY", 40000))
		}
		raw.WriteString(`"}}`)
	}
	raw.WriteString(`]}`)
	fixture := buildMessageInputFixture(test, []byte(raw.String()))
	if fixture.steps < 3 || fixture.generation.Count != count || fixture.generation.Pages.Count != (count+DocMessageInputPageEntries-1)/DocMessageInputPageEntries {
		test.Fatal("large scan did not resume", fixture.steps, fixture.generation)
	}
	for _, body := range fixture.objects {
		if bytes.Contains(body, []byte("PRIVATE_LOCKED_BODY")) {
			test.Fatal("opaque body retained in prepared evidence")
		}
	}
	loads := 0
	last, err := ReadDocMessageInputs(context.Background(), fixture.generation, fixture.generation.Pages.Count-1,
		func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
	if err != nil || len(last) == 0 || last[len(last)-1].Index() != count-1 || loads > 3 {
		test.Fatal("last entry scanned all input evidence", loads, err)
	}
	test.Logf("event_bytes=%d entries=%d pages=%d steps=%d checkpoint_bytes=%d max_raw_read=%d max_step_entries=%d selected_object_loads=%d", raw.Len(), count, fixture.generation.Pages.Count, fixture.steps, fixture.maxCheckpoint, fixture.maxReads, fixture.maxEntries, loads)
}

func TestDocMessageInputBadPublicTypesAndCancelledSteps(test *testing.T) {
	for _, blocks := range []string{`[1]`, `[{"type":1}]`, `[{"type":"text","text":false}]`, `[{"type":"image","name":{}}]`} {
		raw := []byte(`{"kind":"message","blocks":` + blocks + `}`)
		discovery, err := discoverDocFieldReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
		if err != nil {
			test.Fatal(err)
		}
		entry, err := docRangeEntry(0, 0, raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		source, err := discovery.MessageInputSource(entry)
		if err != nil {
			test.Fatal(err)
		}
		state, err := NewDocMessageInputState(source)
		if err != nil {
			test.Fatal(err)
		}
		step, err := StepDocMessageInput(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil }, nil)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocMessageInputStep{}) {
			test.Fatal("malformed public input admitted", blocks, step, err)
		}
	}
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"body"}]}`)
	discovery, _ := discoverDocFieldReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	entry, _ := docRangeEntry(0, 0, raw, 0)
	source, _ := discovery.MessageInputSource(entry)
	state, _ := NewDocMessageInputState(source)
	ctx, cancel := context.WithCancel(context.Background())
	step, err := StepDocMessageInput(ctx, source, state, func(_ context.Context, from, length int) ([]byte, error) {
		cancel()
		return raw[from : from+length], nil
	}, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocMessageInputStep{}) {
		test.Fatal("cancelled scan returned partial entries", err)
	}
}

func FuzzDocMessageInputSourceParity(fuzz *testing.F) {
	for _, value := range []string{"", "\ud55c\uae00🙂", "**first\n\nlast**", "\\\"\r\t", "\u0000\ufeff"} {
		fuzz.Add(value, "image", "**asset**", false)
		fuzz.Add(value, "Text", "", true)
	}
	fuzz.Add("[cxt seed] Branch-switch context: remembered", "image", "asset", false)
	fuzz.Fuzz(func(test *testing.T, text, blockType, name string, summary bool) {
		if len(text)+len(blockType)+len(name) > 4096 || !utf8.ValidString(text) || !utf8.ValidString(blockType) || !utf8.ValidString(name) {
			test.Skip()
		}
		encodedText, _ := json.Marshal(text)
		encodedType, _ := json.Marshal(blockType)
		encodedName, _ := json.Marshal(name)
		mode := "false"
		if summary {
			mode = "true"
		}
		raw := []byte(`{"kind":"message","compact_summary":` + mode + `,"seed_summary":false,"blocks":false,"blo\u0063ks":[{"type":false,"ty\u0070e":"text","text":{},"te\u0078t":` + string(encodedText) + `,"opaque":{"type":"locked","text":"private"}},{"type":` + string(encodedType) + `,"name":` + string(encodedName) + `,"text":` + string(encodedText) + `},{}]}`)
		fixture := buildMessageInputFixture(test, raw)
		inputs := messageFixtureInputs(test, fixture)
		prefix := strings.TrimLeftFunc(text, docRangePresentationSpace)
		wantSummary := summary || strings.HasPrefix(prefix, "[cxt seed] Branch-switch context:") || strings.HasPrefix(prefix, "[cxt] This session was resumed from a branch context seed.")
		if len(inputs) != 3 || fixture.source.Summary != wantSummary || !inputs[0].IsText() || inputs[1].IsText() != (blockType == "text") {
			test.Fatal("fuzz classification changed", len(inputs), fixture.source)
		}
		for index, input := range inputs {
			if !input.SummarySlot() && !input.IsText() {
				selectedType, selectedName, err := input.PlaceholderSources()
				wantType, wantName := "", ""
				if index == 1 {
					wantType, wantName = blockType, name
				}
				if err != nil || messageFixtureString(test, raw, selectedType) != wantType || messageFixtureString(test, raw, selectedName) != wantName {
					test.Fatal("fuzz placeholder selection changed", index, err)
				}
				continue
			}
			selected, err := input.TextSource()
			want := text
			if index == 2 {
				want = ""
			}
			if err != nil || messageFixtureString(test, raw, selected) != want {
				test.Fatal("fuzz text selection changed", index, err)
			}
		}
	})
}
