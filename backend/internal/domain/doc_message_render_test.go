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

func TestDocMessageTextInputsUseIndependentMarkdownPlans(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"**left"},{"type":"text","text":"right**"},{"type":"text","text":"# **한글\uD83D\uDE42** [label](https://hidden.test)"},{"type":"image","name":"asset"}]}`)
	inputFixture := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, inputFixture)
	wants := []string{"**left", "right**", "한글🙂 label"}
	for index, want := range wants {
		source, err := inputs[index].TextSource()
		if err != nil || source == nil {
			test.Fatal("normal text not selected", index, err)
		}
		fixture := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *source))
		cursor, err := NewDocDecodedMarkdownCursor(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.decoded.objects[hash], nil }
		var output strings.Builder
		for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
			fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
			if err != nil {
				test.Fatal(err)
			}
			for offset, character := range fragment.Text() {
				span, err := fragment.LocateSource(context.Background(), offset, utf8.RuneLen(character), load)
				if err != nil || span.From <= source.Token.From || span.To >= source.Token.To {
					test.Fatal("normal text lost its original block", index, span, err)
				}
				if character == '🙂' && string(raw[span.From:span.To]) != `\uD83D\uDE42` {
					test.Fatal("surrogate pair provenance changed", span)
				}
			}
			output.WriteString(fragment.Text())
			cursor = fragment.Next()
		}
		if output.String() != want {
			test.Fatal("normal block crossed Markdown boundary", index, output.String(), want)
		}
	}
}

func TestDocMessageTextLargePublicInputUsesPreparedRanges(test *testing.T) {
	const lines = 12000
	text := strings.Repeat("# **한글🙂**\n", lines)
	raw, err := json.Marshal(map[string]any{"kind": "message", "blocks": []any{map[string]any{"type": "text", "text": text, "opaque": strings.Repeat("LOCKED_PRIVATE_BODY", 10000)}}})
	if err != nil {
		test.Fatal(err)
	}
	inputFixture := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, inputFixture)
	source, err := inputs[0].TextSource()
	if err != nil || source == nil {
		test.Fatal(err)
	}
	fixture := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *source))
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
		if bytes.Contains(body, []byte("LOCKED_PRIVATE_BODY")) {
			test.Fatal("opaque message body retained in prepared input")
		}
		return body, nil
	}
	var output strings.Builder
	for cursor.Render.TextBytes < fixture.plan.Markdown.TextBytes {
		fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), fixture.plan, cursor, load)
		if err != nil || fragment.Next().Render.TextBytes <= cursor.Render.TextBytes {
			test.Fatal("large public text stalled", err)
		}
		for offset, character := range fragment.Text() {
			if character != '🙂' {
				continue
			}
			span, err := fragment.LocateSource(context.Background(), offset, 4, load)
			if err != nil || span.From <= source.Token.From || span.To >= source.Token.To || string(raw[span.From:span.To]) != "🙂" {
				test.Fatal("large public text lost original JSON coordinate", span, err)
			}
			break
		}
		output.WriteString(fragment.Text())
		cursor = fragment.Next()
		fragments++
	}
	want := strings.TrimSuffix(strings.Repeat("한글🙂\n", lines), "\n")
	if output.String() != want || fragments < 3 {
		test.Fatal("large message display changed", output.Len(), len(want), fragments)
	}
	test.Logf("event_bytes=%d decoded_bytes=%d display_bytes=%d input_steps=%d markdown_steps=%d fragments=%d replay_object_loads=%d", len(raw), len(text), output.Len(), inputFixture.steps, fixture.steps, fragments, loads)
}

func messagePlaceholderFixture(test *testing.T, raw []byte) (DocMessagePlaceholderPlan, map[ContentHash][]byte) {
	test.Helper()
	inputs := messageFixtureInputs(test, buildMessageInputFixture(test, raw))
	if len(inputs) != 1 {
		test.Fatal("expected one placeholder input")
	}
	blockType, name, err := inputs[0].PlaceholderSources()
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	var prepared [2]*DocDecodedTextGeneration
	for index, source := range []*DocDecodedTextSource{blockType, name} {
		if source == nil {
			continue
		}
		fixture := buildDecodedTextSourceFixture(test, raw, *source)
		prepared[index] = &fixture.generation
		for hash, body := range fixture.objects {
			objects[hash] = bytes.Clone(body)
		}
	}
	plan, err := NewDocMessagePlaceholderPlan(inputs[0], prepared[0], prepared[1])
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	plan, err = DecodeDocMessagePlaceholderPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return plan, objects
}

func TestDocMessagePlaceholderLiteralTextAndWholeBlockAnchor(test *testing.T) {
	for _, scenario := range []struct{ block, want string }{
		{`{}`, "[]"},
		{`{"type":null,"name":null}`, "[]"},
		{`{"type":"","name":""}`, "[]"},
		{`{"type":"","name":"asset"}`, "[: asset]"},
		{`{"TYPE":"text","text":"not a visible field"}`, "[]"},
		{`{"type":"Text","text":"not a visible text block"}`, "[Text]"},
		{`{"type":"image","name":"**한글🙂** [asset](https://example.test)","text":"hidden","opaque":{"name":"locked"}}`, "[image: **한글🙂** [asset](https://example.test)]"},
		{`{"type":"old","type":null,"name":"old","name":null}`, "[]"},
	} {
		raw := []byte(`{"kind":"message","blocks":[` + scenario.block + `]}`)
		plan, objects := messagePlaceholderFixture(test, raw)
		cursor, err := NewDocMessagePlaceholderCursor(plan)
		if err != nil {
			test.Fatal(err)
		}
		fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, func(_ context.Context, hash ContentHash) ([]byte, error) { return objects[hash], nil })
		if err != nil || fragment.Text() != scenario.want || fragment.From() != 0 || fragment.Next().From != len(scenario.want) {
			test.Fatal("literal placeholder changed", fragment.Text(), scenario.want, err)
		}
		for offset, character := range fragment.Text() {
			span, err := fragment.SourceRange(offset, utf8.RuneLen(character))
			if err != nil || string(raw[span.From:span.To]) != scenario.block {
				test.Fatal("placeholder no longer covers the whole block", span, err)
			}
		}
		projection, err := plan.ProjectionSource()
		if err != nil || projection.EventHash != HashContent(raw) || projection.TextBytes != len(scenario.want) {
			test.Fatal("placeholder origin lost", err)
		}
	}
}

func TestDocMessagePlaceholderLargeNameReadsOnlySelectedPreparedPages(test *testing.T) {
	name := strings.Repeat("**한글🙂** [asset](https://hidden.test)\n", 5000)
	raw, err := json.Marshal(map[string]any{"kind": "message", "blocks": []any{map[string]any{"type": "image", "name": name, "text": "HIDDEN_NON_TEXT_BODY", "opaque": strings.Repeat("LOCKED_PRIVATE_BODY", 10000)}}})
	if err != nil {
		test.Fatal(err)
	}
	plan, objects := messagePlaceholderFixture(test, raw)
	cursor, err := NewDocMessagePlaceholderCursor(plan)
	if err != nil {
		test.Fatal(err)
	}
	loads, maxLoads, fragments := 0, 0, 0
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		body, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		if bytes.Contains(body, []byte("HIDDEN_NON_TEXT_BODY")) || bytes.Contains(body, []byte("LOCKED_PRIVATE_BODY")) {
			test.Fatal("unselected private input copied into prepared objects")
		}
		return body, nil
	}
	var output strings.Builder
	for cursor.From < plan.TextBytes {
		before := loads
		fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, load)
		if err != nil || len(fragment.Text()) > DocDisplayPageTextBytes || !utf8.ValidString(fragment.Text()) || fragment.From() != cursor.From || fragment.Next().From <= cursor.From {
			test.Fatal("large placeholder did not make bounded progress", err)
		}
		maxLoads = max(maxLoads, loads-before)
		output.WriteString(fragment.Text())
		cursor = fragment.Next()
		fragments++
	}
	if output.String() != "[image: "+name+"]" || fragments < 3 || maxLoads > 256 {
		test.Fatal("large placeholder changed or rescanned the whole name", output.Len(), fragments, maxLoads)
	}
	lastCursor := cursor
	lastCursor.From -= 1
	before := loads
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, lastCursor, load)
	if err != nil || fragment.Text() != "]" || loads != before {
		test.Fatal("synthetic last byte scanned the complete name", loads-before, err)
	}
	test.Logf("event_bytes=%d name_bytes=%d fragments=%d max_fragment_object_loads=%d final_literal_object_loads=%d", len(raw), len(name), fragments, maxLoads, loads-before)
}

func TestDocMessagePlaceholderRejectsWrongOriginCorruptionAndCancelledRead(test *testing.T) {
	first, objects := messagePlaceholderFixture(test, []byte(`{"kind":"message","blocks":[{"type":"image","name":"한글🙂"}]}`))
	second, _ := messagePlaceholderFixture(test, []byte(`{"kind":"message","opaque":true,"blocks":[{"type":"image","name":"한글🙂"}]}`))
	cursor, err := NewDocMessagePlaceholderCursor(first)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), second, cursor, func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("cursor switched original message", called, err)
	}
	wrong := first
	wrong.Type = second.Type
	if wrong.Validate() == nil {
		test.Fatal("prepared token from another message accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	fragment, err = ReadDocMessagePlaceholderFragment(ctx, first, cursor, func(_ context.Context, hash ContentHash) ([]byte, error) { cancel(); return objects[hash], nil })
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("cancelled placeholder read returned partial text", err)
	}
	fragment, err = ReadDocMessagePlaceholderFragment(context.Background(), first, cursor, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return append(bytes.Clone(objects[hash]), ' '), nil
	})
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("modified prepared object was accepted", err)
	}
}
