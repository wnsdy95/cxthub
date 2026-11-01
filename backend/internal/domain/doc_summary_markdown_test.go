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

type summaryMarkdownFixture struct {
	input summaryTextFixture
	plan  DocSummaryMarkdownPlan
	steps int
	loads int
}

func buildSummaryMarkdownFixture(test *testing.T, raw []byte) summaryMarkdownFixture {
	test.Helper()
	fixture := summaryMarkdownFixture{input: buildSummaryTextFixture(test, raw)}
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: fixture.input.generation.Hash, Bytes: fixture.input.generation.TextBytes})
	if err != nil {
		test.Fatal(err)
	}
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		fixture.loads++
		return summaryFixtureLoad(fixture.input)(ctx, hash)
	}
	for !state.Done() {
		step, err := StepDocSummaryMarkdownPlan(context.Background(), fixture.input.generation, state, load)
		if err != nil {
			test.Fatal("summary Markdown compile failed", fixture.steps, err)
		}
		fixture.steps++
		for _, object := range step.Objects {
			fixture.input.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode()
		if err != nil {
			test.Fatal(err)
		}
		state, err = DecodeDocMarkdownPlanState(HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
	}
	markdown, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.input.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan = DocSummaryMarkdownPlan{Version: 1, Input: fixture.input.generation, Markdown: markdown}
	encoded, err := fixture.plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.plan, err = DecodeDocSummaryMarkdownPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func assertSummaryMarkdownParity(test *testing.T, raw []byte) {
	test.Helper()
	fixture := buildSummaryMarkdownFixture(test, raw)
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	cursor, err := NewDocSummaryMarkdownCursor(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	var output strings.Builder
	load := summaryFixtureLoad(fixture.input)
	for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
		fragment, err := ReadDocSummaryMarkdownFragment(context.Background(), fixture.plan, cursor, load)
		if err != nil || fragment.From() != cursor.Render.TextBytes || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
			test.Fatal("summary replay stalled", err)
		}
		for offset, character := range fragment.Text() {
			span, err := fragment.LocateSource(context.Background(), offset, utf8.RuneLen(character), load)
			want, failure := baseline.SourceRange(fragment.From()+offset, utf8.RuneLen(character))
			if err != nil || failure != nil || span != want {
				test.Fatal("summary original source differs", fragment.Text(), offset, span, want, err, failure)
			}
		}
		output.WriteString(fragment.Text())
		cursor = fragment.Next()
	}
	if output.String() != baseline.Text() {
		test.Fatal("summary must parse joined slots once", output.String(), baseline.Text())
	}
}

func TestDocSummaryMarkdownCrossSlotConstructsAndProvenance(test *testing.T) {
	for _, blocks := range []string{
		`[]`,
		`[{"text":"**first"},{"type":"image","text":"second**"}]`,
		"[{\"text\":\"```\"},{\"text\":\"**literal**\"},{},{\"text\":\"```\"}]",
		`[{"text":"first"},{},{"text":null},{"text":"last"}]`,
		`[{"text":"# **한글\uD83D\uDE42**"},{"text":"[label](https://hidden.test)"}]`,
		`[{"text":"a\r"},{"text":"b\t"},{"text":"- **c**"}]`,
		`[{"text":"[label](https://hidden"},{"text":".test)"}]`,
		`[{"text":" "},{"text":""},{"text":null},{"text":"🙂"}]`,
	} {
		test.Run(blocks, func(test *testing.T) {
			raw := []byte(`{"kind":"message","compact_summary":true,"blocks":` + blocks + `}`)
			assertSummaryMarkdownParity(test, raw)
		})
	}
	assertSummaryMarkdownParity(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"[cxt seed] Branch-switch context: **first"},{"type":"text","text":"last**"}]}`))
}

func TestDocSummaryMarkdownLargeInputReusesPreparedPages(test *testing.T) {
	const lines = 5000
	text := strings.Repeat("# **한글🙂**\n", lines)
	raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": text, "opaque": strings.Repeat("LOCKED_PRIVATE_BODY", 10000)}, map[string]any{}, map[string]any{"text": "**tail**"}}})
	if err != nil {
		test.Fatal(err)
	}
	fixture := buildSummaryMarkdownFixture(test, raw)
	cursor, err := NewDocSummaryMarkdownCursor(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	var output strings.Builder
	fragments, loads := 0, 0
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return summaryFixtureLoad(fixture.input)(ctx, hash)
	}
	for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
		fragment, err := ReadDocSummaryMarkdownFragment(context.Background(), fixture.plan, cursor, load)
		if err != nil || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
			test.Fatal("large summary replay failed", err)
		}
		output.WriteString(fragment.Text())
		cursor = fragment.Next()
		fragments++
	}
	want := strings.Repeat("한글🙂\n", lines) + "\ntail"
	if output.String() != want || fragments < 3 {
		test.Fatal("large joined summary changed", output.Len(), len(want), fragments)
	}
	test.Logf("event_bytes=%d decoded_bytes=%d display_bytes=%d input_pages=%d input_steps=%d markdown_steps=%d compile_object_loads=%d fragments=%d replay_object_loads=%d", len(raw), fixture.input.generation.TextBytes, output.Len(), fixture.input.generation.Pages.Count, fixture.input.steps, fixture.steps, fixture.loads, fragments, loads)
}

func TestDocSummaryMarkdownCursorCannotSwitchOriginalMessage(test *testing.T) {
	first := buildSummaryMarkdownFixture(test, []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"**same**"}]}`))
	second := buildSummaryMarkdownFixture(test, []byte(`{"kind":"message","compact_summary":true,"opaque":true,"blocks":[{"text":"**same**"}]}`))
	if first.plan.Markdown != second.plan.Markdown {
		test.Fatal("same joined input should share Markdown plan")
	}
	cursor, err := NewDocSummaryMarkdownCursor(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocSummaryMarkdownFragment(context.Background(), second.plan, cursor, func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocSummaryMarkdownFragment{}) {
		test.Fatal("summary cursor switched source message", called, err)
	}
}

func FuzzDocSummaryMarkdownOriginalSourceParity(fuzz *testing.F) {
	for _, value := range []string{"", "**first", "\u0000\r\t", "# 한글🙂", "[label](https://hidden)", "a\n\nb"} {
		fuzz.Add(value, "last**")
	}
	fuzz.Fuzz(func(test *testing.T, first, last string) {
		if len(first)+len(last) > 1024 || !utf8.ValidString(first) || !utf8.ValidString(last) {
			test.Skip()
		}
		raw, err := json.Marshal(map[string]any{"kind": "message", "compact_summary": true, "blocks": []any{map[string]any{"text": first}, map[string]any{}, map[string]any{"text": last}}})
		if err != nil {
			test.Fatal(err)
		}
		assertSummaryMarkdownParity(test, raw)
	})
}
