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

type decodedMarkdownFixture struct {
	decoded decodedTextFixture
	plan    DocDecodedMarkdownPlan
	steps   int
	loads   int
}

func buildDecodedMarkdownFixture(test *testing.T, raw []byte) decodedMarkdownFixture {
	test.Helper()
	decoded := buildDecodedTextFixture(test, raw)
	return buildDecodedMarkdownInputFixture(test, decoded)
}

func buildDecodedMarkdownInputFixture(test *testing.T, decoded decodedTextFixture) decodedMarkdownFixture {
	test.Helper()
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: decoded.generation.Hash, Bytes: decoded.generation.TextBytes})
	if err != nil {
		test.Fatal(err)
	}
	fixture := decodedMarkdownFixture{decoded: decoded}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		fixture.loads++
		body, found := fixture.decoded.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Done() {
		fixture.steps++
		step, err := StepDocDecodedMarkdownPlan(context.Background(), decoded.generation, state, load)
		if err != nil {
			test.Fatal("Markdown from verified decoded pages failed", fixture.steps, err)
		}
		for _, object := range step.Objects {
			fixture.decoded.objects[object.Hash] = bytes.Clone(object.Bytes)
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
	plan, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.decoded.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan = DocDecodedMarkdownPlan{Version: 1, Input: decoded.generation, Markdown: plan}
	if err := fixture.plan.Validate(); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestDocDecodedMarkdownOriginalJSONCoordinates(test *testing.T) {
	for _, token := range []string{
		`"# A  ** B**\n\n[한글](https://hidden.test) ` + "`🙂`" + `"`,
		`"a\n\nb"`,
		`"**a\uD83D\uDE42** [label](https://hidden)"`,
		`"- \uD800\t**X**\n1. \uDC00"`,
		"\"```\\nraw\\r\\n**literal**\\n```\"",
	} {
		raw := []byte(`{"kind":"reasoning","opaque":{"secret":"locked original"},"redacted_summary":` + token + `}`)
		fixture := buildDecodedMarkdownFixture(test, raw)
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.decoded.objects[hash], nil }
		var got strings.Builder
		for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
			fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
			if err != nil || fragment.From() != cursor.Render.TextBytes || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
				test.Fatal("bridge did not progress", fragment.Next(), err)
			}
			for offset, character := range fragment.Text() {
				actual, err := fragment.LocateSource(context.Background(), offset, utf8.RuneLen(character), load)
				want, failure := baseline.SourceRange(fragment.From()+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || actual != want || actual.From <= fixture.plan.Input.Source.Token.From || actual.To >= fixture.plan.Input.Source.Token.To {
					test.Fatal("original JSON coordinate changed", token, offset, actual, want, err, failure)
				}
			}
			got.WriteString(fragment.Text())
			cursor = fragment.Next()
		}
		if got.String() != baseline.Text() {
			test.Fatal("display text changed", got.String(), baseline.Text())
		}
	}
}

func TestDocDecodedMarkdownLargeEscapedInputUsesOnlyPreparedObjects(test *testing.T) {
	text := strings.Repeat("# **한글🙂** [label](https://hidden.test)\n", 5000)
	raw, err := json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": text, "opaque": strings.Repeat("LOCKED_PRIVATE_STATE", 10000)})
	if err != nil {
		test.Fatal(err)
	}
	fixture := buildDecodedMarkdownFixture(test, raw)
	want := strings.TrimSuffix(strings.Repeat("한글🙂 label\n", 5000), "\n")
	cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	loads, fragments := 0, 0
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		body, found := fixture.decoded.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	var got strings.Builder
	for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
		fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
		if err != nil {
			test.Fatal("large bridge read failed", err)
		}
		got.WriteString(fragment.Text())
		if len(fragment.Text()) > DocDisplayPageTextBytes || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
			test.Fatal("unbounded or stalled output")
		}
		for offset, character := range fragment.Text() {
			if character != '🙂' {
				continue
			}
			span, err := fragment.LocateSource(context.Background(), offset, 4, load)
			if err != nil || string(raw[span.From:span.To]) != "🙂" {
				test.Fatal("large original coordinate lost", span, err)
			}
			break
		}
		cursor = fragment.Next()
		fragments++
	}
	if got.String() != want {
		test.Fatal("large Markdown body changed", got.Len(), len(want))
	}
	if fixture.loads > 2000 || loads > 2000 {
		test.Fatal("per-operation decoded page reuse regressed", fixture.loads, loads)
	}
	test.Logf("event_bytes=%d decoded_bytes=%d displayed_bytes=%d decoded_pages=%d plan_steps=%d compile_object_loads=%d output_fragments=%d replay_object_loads=%d", len(raw), len(text), got.Len(), fixture.decoded.generation.Pages.Count, fixture.steps, fixture.loads, fragments, loads)
}

func TestDocDecodedMarkdownCursorBindsOriginEvenWhenTextAndPlanAreReusable(test *testing.T) {
	first := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**same**","opaque":"A"}`))
	second := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","opaque":"different origin","redacted_summary":"**same**"}`))
	if first.plan.Markdown != second.plan.Markdown {
		test.Fatal("identical decoded text should share a Markdown plan")
	}
	cursor, err := NewDocDecodedMarkdownCursor(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), second.plan, cursor,
		func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocDecodedMarkdownFragment{}) {
		test.Fatal("cursor switched original event", called, err)
	}
	encoded, err := first.plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocDecodedMarkdownPlan(HashContent(encoded), encoded); err != nil || decoded != first.plan {
		test.Fatal("bridge plan codec failed", err)
	}
	projection, err := first.plan.ProjectionSource()
	if err != nil || projection.EventHash != first.decoded.source.EventHash || projection.TextBytes != first.plan.Markdown.TextBytes || projection.Plan != HashContent(encoded) {
		test.Fatal("projection lost original binding", projection, err)
	}
}

func FuzzDocDecodedMarkdownOriginalSourceParity(fuzz *testing.F) {
	for _, source := range []string{"", "# A  ** B**", "a\n\nb", "[한글](https://hidden.test)", "`🙂`", "x\t\n\r\u00a0", "**a\nb**"} {
		fuzz.Add(source)
	}
	fuzz.Fuzz(func(test *testing.T, source string) {
		if len(source) > 2048 || !utf8.ValidString(source) {
			test.Skip()
		}
		raw, err := json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": source, "opaque": map[string]any{"redacted_summary": "locked private"}})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildDecodedMarkdownFixture(test, raw)
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.decoded.objects[hash], nil }
		var output strings.Builder
		for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
			fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
			if err != nil || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
				test.Fatal("fuzz bridge did not advance", err)
			}
			checked := 0
			for offset, character := range fragment.Text() {
				actual, err := fragment.LocateSource(context.Background(), offset, utf8.RuneLen(character), load)
				want, failure := baseline.SourceRange(fragment.From()+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || actual != want {
					test.Fatal("fuzz original JSON cover changed", source, offset, actual, want, err, failure)
				}
				checked++
				if checked == 32 {
					break
				}
			}
			output.WriteString(fragment.Text())
			cursor = fragment.Next()
		}
		if output.String() != baseline.Text() {
			test.Fatal("fuzz display changed", source, output.String(), baseline.Text())
		}
	})
}
