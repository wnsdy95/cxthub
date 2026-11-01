package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

type displayAssemblyFixture struct {
	generation DocDisplayProjectionGeneration
	objects    map[ContentHash][]byte
	steps      int
	maxState   int
	maxLoads   int
}

func assembleDisplayFixture(test *testing.T, input DocDisplayAssemblyInput, objects map[ContentHash][]byte) displayAssemblyFixture {
	test.Helper()
	state, err := NewDocDisplayAssemblyState(input)
	if err != nil {
		test.Fatal(err)
	}
	fixture := displayAssemblyFixture{objects: objects}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Output.Done {
		before, err := state.Encode(input)
		if err != nil {
			test.Fatal(err)
		}
		loads := 0
		step, err := StepDocDisplayAssembly(context.Background(), input, state, func(ctx context.Context, hash ContentHash) ([]byte, error) { loads++; return load(ctx, hash) })
		if err != nil {
			test.Fatal("real assembly failed", fixture.steps, state.Output.TextBytes, state.Pending, err)
		}
		fixture.steps++
		fixture.maxLoads = max(fixture.maxLoads, loads)
		after, err := state.Encode(input)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("assembly changed previous checkpoint", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("bad assembled staging object")
			}
			if existing, found := objects[object.Hash]; found && !bytes.Equal(existing, object.Bytes) {
				test.Fatal("immutable assembly object changed")
			}
			objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(input)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("assembly checkpoint failed to progress or retained text", err)
		}
		fixture.maxState = max(fixture.maxState, len(encoded))
		state, err = DecodeDocDisplayAssemblyState(input, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
		if err := ValidateDocDisplayAssemblyCheckpoint(context.Background(), input, state, load); err != nil {
			test.Fatal("real assembled checkpoint invalid", err)
		}
	}
	generation, produced, err := FinishDocDisplayAssembly(context.Background(), input, state, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range produced {
		objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.generation, err = DecodeDocDisplayProjectionGeneration(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func assembledFixturePages(test *testing.T, fixture displayAssemblyFixture, check func(DocDisplayPage)) string {
	test.Helper()
	var output strings.Builder
	for index := 0; index < fixture.generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), fixture.generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
		if err != nil || page.From != output.Len() || page.ValidateProjection(fixture.generation.Source) != nil {
			test.Fatal("invalid complete display page", index, err)
		}
		if index < fixture.generation.Pages.Count-1 && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
			test.Fatal("short staging fragment published as complete page")
		}
		if check != nil {
			check(page)
		}
		output.WriteString(page.Text)
	}
	if output.Len() != fixture.generation.Source.TextBytes {
		test.Fatal("assembled display length changed", output.Len(), fixture.generation.Source.TextBytes)
	}
	return output.String()
}

func TestDocDisplayAssemblyRealPlansMatchWholeEventOracle(test *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"kind":"reasoning","redacted_summary":"# **한글\uD83D\uDE42** [label](https://hidden.test)"}`),
		[]byte(`{"kind":"reasoning","redacted_summary":" \r\n\t "}`),
		[]byte(`{"kind":"message","blocks":[{"type":"text","text":"# **한글\uD83D\uDE42** [label](https://hidden.test)","opaque":"private"}]}`),
		[]byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"**first"},{},{"text":"last**"}]}`),
		[]byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":""},{"text":null}]}`),
		[]byte(`{"kind":"message","blocks":[{"type":"image","name":"**한글🙂**","text":"not visible"}]}`),
	} {
		var input DocDisplayAssemblyInput
		var objects map[ContentHash][]byte
		var err error
		if bytes.Contains(raw, []byte(`"kind":"reasoning"`)) {
			compiled := buildDecodedMarkdownFixture(test, raw)
			input, err = NewDocDecodedMarkdownAssemblyInput(compiled.plan)
			objects = compiled.decoded.objects
		} else if bytes.Contains(raw, []byte(`"compact_summary":true`)) {
			compiled := buildSummaryMarkdownFixture(test, raw)
			input, err = NewDocSummaryMarkdownAssemblyInput(compiled.plan)
			objects = compiled.input.objects
		} else if bytes.Contains(raw, []byte(`"type":"text"`)) {
			message := buildMessageInputFixture(test, raw)
			selected, failure := messageFixtureInputs(test, message)[0].TextSource()
			if failure != nil || selected == nil {
				test.Fatal(failure)
			}
			compiled := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *selected))
			input, err = NewDocDecodedMarkdownAssemblyInput(compiled.plan)
			objects = compiled.decoded.objects
		} else {
			plan, staged := messagePlaceholderFixture(test, raw)
			input, err = NewDocMessagePlaceholderAssemblyInput(plan)
			objects = staged
		}
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		fixture := assembleDisplayFixture(test, input, objects)
		got := assembledFixturePages(test, fixture, func(page DocDisplayPage) {
			for offset, character := range page.Text {
				span, err := page.SourceRange(offset, utf8.RuneLen(character))
				want, failure := baseline.SourceRange(page.From+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || span != want {
					test.Fatal("assembled source differs", span, want, err, failure)
				}
			}
		})
		if got != baseline.Text() {
			test.Fatal("assembled display differs", got, baseline.Text())
		}
	}
}

func TestDocDisplayAssemblyLargeEscapedReasoningAndLatePage(test *testing.T) {
	const count = 40000
	raw := []byte(`{"kind":"reasoning","opaque":"` + strings.Repeat("LOCKED_PRIVATE_BODY", 10000) + `","redacted_summary":"**` + strings.Repeat(`\u0061`, count) + `**"}`)
	compiled := buildDecodedMarkdownFixture(test, raw)
	input, err := NewDocDecodedMarkdownAssemblyInput(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	fixture := assembleDisplayFixture(test, input, compiled.decoded.objects)
	got := assembledFixturePages(test, fixture, func(page DocDisplayPage) {
		span, err := page.SourceRange(0, 1)
		if err != nil || string(raw[span.From:span.To]) != `\u0061` {
			test.Fatal("assembled escaped cover changed", span, err)
		}
	})
	if got != strings.Repeat("a", count) || fixture.maxLoads > 80 {
		test.Fatal("large escaped assembly changed or exceeded bounded reads", len(got), fixture.maxLoads)
	}
	loads := 0
	last, err := ReadDocDisplayProjectionPage(context.Background(), fixture.generation, fixture.generation.Pages.Count-1, func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
	if err != nil || len(last.Text) == 0 || loads > 5 {
		test.Fatal("final assembled page scanned preceding display", loads, err)
	}
	test.Logf("event_bytes=%d displayed_bytes=%d steps=%d pages=%d checkpoint_bytes=%d max_step_object_loads=%d final_page_object_loads=%d", len(raw), len(got), fixture.steps, fixture.generation.Pages.Count, fixture.maxState, fixture.maxLoads, loads)
}

func TestDocDisplayAssemblyLargeSummaryAndPlaceholder(test *testing.T) {
	const lines = 5000
	text := strings.Repeat("# **한글🙂**\n", lines)
	raw, _ := json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": text}, map[string]any{}, map[string]any{"text": "**tail**"}}})
	compiled := buildSummaryMarkdownFixture(test, raw)
	input, err := NewDocSummaryMarkdownAssemblyInput(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	summary := assembleDisplayFixture(test, input, compiled.input.objects)
	if got := assembledFixturePages(test, summary, nil); got != strings.Repeat("한글🙂\n", lines)+"\ntail" {
		test.Fatal("large summary pages changed", len(got))
	}
	name := strings.Repeat("PUBLIC_BODY_MARKER **한글🙂**\n", 4000)
	raw, _ = json.Marshal(map[string]any{"kind": "message", "blocks": []any{map[string]any{"type": "image", "name": name}}})
	plan, objects := messagePlaceholderFixture(test, raw)
	input, err = NewDocMessagePlaceholderAssemblyInput(plan)
	if err != nil {
		test.Fatal(err)
	}
	placeholder := assembleDisplayFixture(test, input, objects)
	if got := assembledFixturePages(test, placeholder, nil); got != "[image: "+name+"]" {
		test.Fatal("large literal placeholder pages changed", len(got))
	}
	test.Logf("summary_bytes=%d summary_steps=%d summary_pages=%d placeholder_bytes=%d placeholder_steps=%d placeholder_pages=%d max_placeholder_checkpoint=%d", summary.generation.Source.TextBytes, summary.steps, summary.generation.Pages.Count, placeholder.generation.Source.TextBytes, placeholder.steps, placeholder.generation.Pages.Count, placeholder.maxState)
}

func FuzzDocDisplayAssemblyWholeEventParity(fuzz *testing.F) {
	for _, text := range []string{"", "# **한글🙂**", "a\n\nb", "\u0000\r\t", "[label](https://hidden.test)"} {
		fuzz.Add(text, false)
		fuzz.Add(text, true)
	}
	fuzz.Fuzz(func(test *testing.T, text string, summary bool) {
		if len(text) > 1024 || !utf8.ValidString(text) {
			test.Skip()
		}
		var raw []byte
		var input DocDisplayAssemblyInput
		var objects map[ContentHash][]byte
		var err error
		if summary {
			raw, err = json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": text}, map[string]any{}, map[string]any{"text": "tail"}}})
			if err != nil {
				test.Fatal(err)
			}
			fixture := buildSummaryMarkdownFixture(test, raw)
			input, err = NewDocSummaryMarkdownAssemblyInput(fixture.plan)
			objects = fixture.input.objects
		} else {
			raw, err = json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": text})
			if err != nil {
				test.Fatal(err)
			}
			fixture := buildDecodedMarkdownFixture(test, raw)
			input, err = NewDocDecodedMarkdownAssemblyInput(fixture.plan)
			objects = fixture.decoded.objects
		}
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		fixture := assembleDisplayFixture(test, input, objects)
		output := assembledFixturePages(test, fixture, func(page DocDisplayPage) {
			for offset, character := range page.Text {
				got, err := page.SourceRange(offset, utf8.RuneLen(character))
				want, failure := baseline.SourceRange(page.From+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || got != want {
					test.Fatal("fuzz assembled source changed", got, want, err, failure)
				}
			}
		})
		if output != baseline.Text() {
			test.Fatal("fuzz assembled text changed", output, baseline.Text())
		}
	})
}
