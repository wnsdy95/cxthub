package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type summaryTextFixture struct {
	source        DocSummaryTextSource
	state         DocSummaryTextState
	generation    DocSummaryTextGeneration
	objects       map[ContentHash][]byte
	steps         int
	maxCheckpoint int
	maxReads      int
	maxSlots      int
}

func buildSummaryTextFixture(test *testing.T, raw []byte) summaryTextFixture {
	test.Helper()
	message := buildMessageInputFixture(test, raw)
	source := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: message.generation}
	state, err := NewDocSummaryTextState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := summaryTextFixture{source: source, objects: message.objects}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Done {
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		reads := 0
		step, err := StepDocSummaryText(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			if from <= source.Message.Source.Blocks.From || length <= 0 || length > 4096 || from > source.Message.Source.Blocks.To-length {
				test.Fatal("summary input overread", from, length)
			}
			reads += length
			return raw[from : from+length], nil
		}, load)
		if err != nil {
			test.Fatal("summary step failed", fixture.steps, state.Block, state.Next, state.TextBytes, err)
		}
		if reads != step.ReadBytes || reads > DocDisplayStepSourceBytes || step.State.Block-state.Block > DocSummaryTextStepSlots {
			test.Fatal("summary budget exceeded", reads, step.ReadBytes, step.State.Block-state.Block)
		}
		fixture.steps++
		fixture.maxReads, fixture.maxSlots = max(fixture.maxReads, reads), max(fixture.maxSlots, step.State.Block-state.Block)
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("summary mutated previous checkpoint", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid summary staging object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(source)
		if err != nil || bytes.Equal(encoded, before) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("summary checkpoint failed to progress or retained body", err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		state, err = DecodeDocSummaryTextState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("summary checkpoint restart failed", err)
		}
		if err := ValidateDocSummaryTextCheckpoint(context.Background(), source, state, load); err != nil {
			test.Fatal("summary staged checkpoint invalid", state.Block, state.Next, state.TextBytes, err)
		}
	}
	generation, objects, err := FinishDocSummaryText(context.Background(), source, state, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.generation, err = DecodeDocSummaryTextGeneration(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	fixture.state = state
	return fixture
}

func summaryFixtureLoad(fixture summaryTextFixture) func(context.Context, ContentHash) ([]byte, error) {
	return func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
}

func summaryFixtureBytes(test *testing.T, fixture summaryTextFixture) string {
	test.Helper()
	var output strings.Builder
	for from := 0; from < fixture.generation.TextBytes; {
		length := min(4096, fixture.generation.TextBytes-from)
		raw, err := ReadDocSummaryTextBytes(context.Background(), fixture.generation, from, length, summaryFixtureLoad(fixture))
		if err != nil || len(raw) != length {
			test.Fatal("summary range read failed", from, length, err)
		}
		output.Write(raw)
		from += length
	}
	if HashContent([]byte(output.String())) != fixture.generation.Hash {
		test.Fatal("summary decoded digest mismatch")
	}
	return output.String()
}

func TestDocSummaryTextKeepsEmptySlotsAndOriginalJoinAnchors(test *testing.T) {
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"type":"text","text":"first"},{"type":"image"},{"text":null},{"text":""},{"text":"\uD83D\uDE42last"}]}`)
	fixture := buildSummaryTextFixture(test, raw)
	want := "first\n\n\n\n🙂last"
	if got := summaryFixtureBytes(test, fixture); got != want {
		test.Fatal("summary slots were filtered", got, want)
	}
	message := messageFixtureInputs(test, buildMessageInputFixture(test, raw))
	for index, offset := range []int{5, 6, 7, 8} {
		span, err := LocateDocSummaryTextSource(context.Background(), fixture.generation, offset, 1, summaryFixtureLoad(fixture))
		if err != nil {
			test.Fatal(err)
		}
		if index < 3 && span != message[index+1].Anchor() || index == 3 && string(raw[span.From:span.To]) != `\uD83D\uDE42` {
			test.Fatal("summary synthetic newline anchor changed", index, span)
		}
	}
}

func TestDocSummaryTextEmptyAndNullMessages(test *testing.T) {
	for _, blocks := range []string{"", `,"blocks":null`, `,"blocks":[]`, `,"blocks":[{}]`, `,"blocks":[{"text":""}]`} {
		raw := []byte(`{"kind":"message","compact_summary":true` + blocks + `}`)
		fixture := buildSummaryTextFixture(test, raw)
		if fixture.generation.TextBytes != 0 || fixture.generation.Hash != HashContent(nil) || fixture.generation.Pages.Count != 0 || fixture.generation.Lookup.Count != 0 {
			test.Fatal("empty summary gained output", fixture.generation)
		}
	}
}

func TestDocSummaryTextRejectsImpossibleZeroInputStates(test *testing.T) {
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"body"}]}`)
	message := buildMessageInputFixture(test, raw)
	source := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: message.generation}
	state, err := NewDocSummaryTextState(source)
	if err != nil {
		test.Fatal(err)
	}
	selected, err := messageFixtureInputs(test, message)[0].TextSource()
	if err != nil {
		test.Fatal(err)
	}
	state.Joined, state.Next = true, selected.Token.From+2
	if state.Validate(source) == nil {
		test.Fatal("zero-output checkpoint skipped into nonempty token")
	}
	empty := buildMessageInputFixture(test, []byte(`{"kind":"message","compact_summary":true,"blocks":[ ]}`))
	emptySource := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: empty.generation}
	forged := DocSummaryTextGeneration{Version: 1, Source: emptySource, Hash: HashContent([]byte("x")), TextBytes: 1, Pages: DocRangeDirectoryRoot{Hash: HashContent(nil), Count: 1}, Lookup: DocRangeDirectoryRoot{Hash: HashContent(nil), Count: 1}}
	if forged.Validate() == nil {
		test.Fatal("zero-slot source gained a nonempty generation")
	}
	hash, err := emptySource.Hash()
	if err != nil {
		test.Fatal(err)
	}
	page := DocDisplayPage{Version: 4, Source: hash, Text: "x", Runs: []DocDisplayRun{{To: 1, SourceFrom: emptySource.Message.Source.Blocks.From + 1, SourceTo: emptySource.Message.Source.Blocks.From + 2, Linear: true}}}
	if _, err := encodeDocSummaryTextPage(emptySource, page); !errors.Is(err, ErrIntegrity) {
		test.Fatal("zero-slot source accepted a data page", err)
	}
}

func TestDocSummaryTextManyEmptySlotsUsesBoundedPages(test *testing.T) {
	const count = 10000
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[` + strings.Repeat(`{},`, count-1) + `{}]}`)
	fixture := buildSummaryTextFixture(test, raw)
	if got := summaryFixtureBytes(test, fixture); got != strings.Repeat("\n", count-1) || fixture.steps < count/DocSummaryTextStepSlots {
		test.Fatal("empty-slot composition changed", len(got), fixture.steps)
	}
	loads := 0
	last, err := ReadDocSummaryTextBytes(context.Background(), fixture.generation, fixture.generation.TextBytes-1, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return summaryFixtureLoad(fixture)(ctx, hash)
	})
	if err != nil || string(last) != "\n" || loads > 6 {
		test.Fatal("last newline scanned all preceding slots", loads, err)
	}
	test.Logf("event_bytes=%d slots=%d decoded_bytes=%d pages=%d steps=%d checkpoint_bytes=%d max_step_slots=%d selected_object_loads=%d", len(raw), count, fixture.generation.TextBytes, fixture.generation.Pages.Count, fixture.steps, fixture.maxCheckpoint, fixture.maxSlots, loads)
}

func TestDocSummaryTextJoinOnlyCheckpointAcrossPageBoundary(test *testing.T) {
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[` + strings.Repeat(`{},`, DocDisplayPageRuns) + `{"text":"🙂tail"}]}`)
	fixture := buildSummaryTextFixture(test, raw)
	want := strings.Repeat("\n", DocDisplayPageRuns) + "🙂tail"
	if got := summaryFixtureBytes(test, fixture); got != want || fixture.generation.Pages.Count != 2 {
		test.Fatal("joined-only boundary failed to resume", len(got), fixture.generation.Pages.Count)
	}
	span, err := LocateDocSummaryTextSource(context.Background(), fixture.generation, DocDisplayPageRuns-1, 5, summaryFixtureLoad(fixture))
	if err != nil || string(raw[span.From:span.To]) != "🙂" {
		test.Fatal("cross-page LF and first rune lost their shared original anchor", span, err)
	}
}

func TestDocSummaryTextLargeEscapedAndOpaqueInputs(test *testing.T) {
	text := strings.Repeat("PUBLIC_BODY_MARKER **한글🙂**\n", 5000)
	raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": text, "opaque": strings.Repeat("LOCKED_PRIVATE_BODY", 20000)}, map[string]any{}, map[string]any{"text": "tail"}}})
	if err != nil {
		test.Fatal(err)
	}
	fixture := buildSummaryTextFixture(test, raw)
	if got := summaryFixtureBytes(test, fixture); got != text+"\n\ntail" {
		test.Fatal("large summary changed", len(got))
	}
	for _, object := range fixture.objects {
		if bytes.Contains(object, []byte("LOCKED_PRIVATE_BODY")) {
			test.Fatal("opaque state retained by summary preparation")
		}
	}
	test.Logf("event_bytes=%d decoded_bytes=%d pages=%d steps=%d checkpoint_bytes=%d max_raw_read=%d", len(raw), fixture.generation.TextBytes, fixture.generation.Pages.Count, fixture.steps, fixture.maxCheckpoint, fixture.maxReads)
}

func TestDocSummaryTextCancellationCorruptionAndProfileIsolation(test *testing.T) {
	message := buildMessageInputFixture(test, []byte(`{"kind":"message","blocks":[{"text":"body"}]}`))
	invalid := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: message.generation}
	if invalid.Validate() == nil {
		test.Fatal("normal message accepted as summary input")
	}
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"body"},{"text":"tail"}]}`)
	fixture := buildSummaryTextFixture(test, raw)
	initial, err := NewDocSummaryTextState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	step, err := StepDocSummaryText(ctx, fixture.source, initial, func(_ context.Context, from, length int) ([]byte, error) {
		cancel()
		return raw[from : from+length], nil
	}, summaryFixtureLoad(fixture))
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocSummaryTextStep{}) {
		test.Fatal("cancelled summary returned partial state", err)
	}
	var encodedPage []byte
	for _, object := range fixture.objects {
		var page DocDisplayPage
		if json.Unmarshal(object, &page) == nil && page.Version == 4 {
			encodedPage = object
			break
		}
	}
	if len(encodedPage) == 0 {
		test.Fatal("summary page missing")
	}
	changed := append(bytes.Clone(encodedPage), ' ')
	if _, err := DecodeDocSummaryTextPage(fixture.source, HashContent(changed), changed); !errors.Is(err, ErrIntegrity) {
		test.Fatal("noncanonical summary page accepted", err)
	}
	if _, err := ReadDocSummaryTextBytes(context.Background(), fixture.generation, 0, 1, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return append(bytes.Clone(fixture.objects[hash]), ' '), nil
	}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("corrupt prepared summary object accepted", err)
	}
}

func TestDocSummaryTextHashTailsAndUTF8ByteRanges(test *testing.T) {
	for length := 0; length < 64; length++ {
		first, _ := json.Marshal(strings.Repeat("a", length) + "🙂")
		raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":` + string(first) + `},` + strings.Repeat(`{},`, 130) + `{}]}`)
		fixture := buildSummaryTextFixture(test, raw)
		want := strings.Repeat("a", length) + "🙂" + strings.Repeat("\n", 131)
		if got := summaryFixtureBytes(test, fixture); got != want {
			test.Fatal("summary hash tail changed", length)
		}
		body, err := ReadDocSummaryTextBytes(context.Background(), fixture.generation, length+1, 2, summaryFixtureLoad(fixture))
		if err != nil || !bytes.Equal(body, []byte("🙂")[1:3]) {
			test.Fatal("internal byte range must support UTF-8 interior offsets", err)
		}
		if _, err := LocateDocSummaryTextSource(context.Background(), fixture.generation, length+1, 2, summaryFixtureLoad(fixture)); !errors.Is(err, ErrValidation) {
			test.Fatal("source coordinates accepted partial rune", err)
		}
	}
}

func FuzzDocSummaryTextSlotParity(fuzz *testing.F) {
	for _, value := range []string{"", "🙂", "**left\nright**", "\u0000\r\t", "[label](https://hidden)"} {
		fuzz.Add(value, "tail")
	}
	fuzz.Fuzz(func(test *testing.T, first, last string) {
		if len(first)+len(last) > 2048 || !utf8.ValidString(first) || !utf8.ValidString(last) {
			test.Skip()
		}
		raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": first}, map[string]any{}, map[string]any{"text": last}}})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildSummaryTextFixture(test, raw)
		if got := summaryFixtureBytes(test, fixture); got != first+"\n\n"+last {
			test.Fatal("summary slot bytes differ", got)
		}
	})
}
