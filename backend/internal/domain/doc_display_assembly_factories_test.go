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

func assertMappedAssemblyReplay(test *testing.T, input DocDisplayAssemblyInput, objects map[ContentHash][]byte, baseline DocFindDisplayText) {
	test.Helper()
	cursor := bytes.Clone(input.initial)
	var output strings.Builder
	for output.Len() < input.source.TextBytes {
		loads := 0
		page, next, err := input.read(context.Background(), cursor, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			body, found := objects[hash]
			if !found {
				return nil, ErrNotFound
			}
			return body, nil
		})
		position, positionErr := input.cursor(next)
		if err != nil || positionErr != nil || position != output.Len()+len(page.Text) || page.From != output.Len() || len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || len(page.Runs) > DocDisplayPageRuns || loads > DocDisplayMappedReplayObjects {
			test.Fatal("mapped replay failed to make bounded progress", position, len(page.Text), loads, err, positionErr)
		}
		for offset, character := range page.Text {
			got, err := page.SourceRange(offset, utf8.RuneLen(character))
			want, failure := baseline.SourceRange(page.From+offset, utf8.RuneLen(character))
			if err != nil || failure != nil || got != want {
				test.Fatal("mapped replay changed original JSON coordinates", offset, got, want, err, failure)
			}
		}
		output.WriteString(page.Text)
		cursor = next
	}
	if output.String() != baseline.Text() {
		test.Fatal("mapped replay changed displayed text", output.String(), baseline.Text())
	}
}

func TestDocDisplayAssemblyFactoriesPreserveRealTextAndOriginalCoordinates(test *testing.T) {
	for _, token := range []string{
		`"# A  **한글\uD83D\uDE42** [label](https://hidden.test)"`,
		`"a\n\nb\n\n- \uD800\t**X**"`,
		"\"```\\nraw\\r\\n**literal**\\n```\"",
		`" \t\n\r "`,
	} {
		raw := []byte(`{"kind":"reasoning","opaque":"locked","redacted_summary":` + token + `}`)
		fixture := buildDecodedMarkdownFixture(test, raw)
		input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		assertMappedAssemblyReplay(test, input, fixture.decoded.objects, baseline)
	}
	for _, blocks := range []string{
		`[{"text":"**first"},{},{"text":"last**"}]`,
		"[{\"text\":\"```\"},{\"text\":\"**literal**\"},{},{\"text\":\"```\"}]",
		`[{"text":"# **한글\uD83D\uDE42**"},{"text":"a\r\nb"}]`,
		`[{"text":"[label](https://hidden"},{"text":".test)"}]`,
		`[]`,
	} {
		raw := []byte(`{"kind":"message","compact_summary":true,"blocks":` + blocks + `}`)
		fixture := buildSummaryMarkdownFixture(test, raw)
		input, err := NewDocSummaryMarkdownAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		assertMappedAssemblyReplay(test, input, fixture.input.objects, baseline)
	}
	raw := []byte(`{"kind":"message","blocks":[{"type":"image","name":"**한글🙂**","text":"not displayed"}]}`)
	plan, objects := messagePlaceholderFixture(test, raw)
	input, err := NewDocMessagePlaceholderAssemblyInput(plan)
	if err != nil {
		test.Fatal(err)
	}
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	assertMappedAssemblyReplay(test, input, objects, baseline)
}

func TestDocDisplayMappedReplayEscapedInputUsesBoundedObjectLoads(test *testing.T) {
	const count = 10000
	raw := []byte(`{"kind":"reasoning","opaque":"locked","redacted_summary":"**` + strings.Repeat(`\u0061`, count) + `**"}`)
	fixture := buildDecodedMarkdownFixture(test, raw)
	input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	cursor := bytes.Clone(input.initial)
	var output strings.Builder
	maxLoads, fragments := 0, 0
	for output.Len() < count {
		loads := 0
		page, next, err := input.read(context.Background(), cursor, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return fixture.decoded.objects[hash], nil
		})
		if err != nil || loads > DocDisplayMappedReplayObjects || len(page.Runs) > DocDisplayPageRuns || len(page.Text) == 0 {
			test.Fatal("escaped original mapping exceeded bounds", loads, len(page.Runs), err)
		}
		span, err := page.SourceRange(0, 1)
		if err != nil || string(raw[span.From:span.To]) != `\u0061` {
			test.Fatal("escaped original coordinate changed", span, err)
		}
		maxLoads = max(maxLoads, loads)
		fragments++
		output.WriteString(page.Text)
		cursor = next
	}
	if output.String() != strings.Repeat("a", count) || fragments < count/DocDisplayPageRuns {
		test.Fatal("escaped mapped text changed", output.Len(), fragments)
	}
	test.Logf("event_bytes=%d displayed_bytes=%d fragments=%d max_object_loads=%d", len(raw), count, fragments, maxLoads)
}

func TestDocDisplayMappedReplayRespectsSmallByteAndSourceRunLimits(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"\u0061\u0062x\uD83D\uDE42y\u0063\u0064z"}`)
	fixture := buildDecodedMarkdownFixture(test, raw)
	input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	cursor := bytes.Clone(input.initial)
	var output strings.Builder
	for output.Len() < input.source.TextBytes {
		page, next, err := input.read(context.Background(), cursor, 7, 2, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.decoded.objects[hash], nil })
		position, failure := input.cursor(next)
		if err != nil || failure != nil || len(page.Text) > 7 || len(page.Runs) > 2 || !utf8.ValidString(page.Text) || position <= output.Len() {
			test.Fatal("small mapped limits did not make valid progress", page.Text, len(page.Runs), position, err, failure)
		}
		output.WriteString(page.Text)
		cursor = next
	}
	if output.String() != "abx🙂ycdz" {
		test.Fatal("limited mapped replay changed text", output.String())
	}
}

func TestDocDisplayAssemblyFactoryRejectsForeignCursorAndCancelledMapping(test *testing.T) {
	first := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**same**","opaque":"A"}`))
	second := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","opaque":"different origin","redacted_summary":"**same**"}`))
	left, err := NewDocDecodedMarkdownAssemblyInput(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	right, err := NewDocDecodedMarkdownAssemblyInput(second.plan)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	page, next, err := right.read(context.Background(), left.initial, 1024, 100, func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("mapped cursor changed source before I/O", called, err)
	}
	for _, changed := range [][]byte{append(bytes.Clone(left.initial), ' '), append([]byte(`{"unknown":true,`), left.initial[1:]...)} {
		if _, err := left.cursor(changed); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical native cursor accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	page, next, err = left.read(ctx, left.initial, 1024, 100, func(_ context.Context, hash ContentHash) ([]byte, error) {
		cancel()
		return first.decoded.objects[hash], nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("cancelled mapped replay exposed partial output", err)
	}
}

func FuzzDocDisplayMappedReplaySourceParity(fuzz *testing.F) {
	for _, source := range []string{"", "# **한글🙂**", "a\n\nb", "[label](https://hidden)", "\u0000\r\t"} {
		fuzz.Add(source)
	}
	fuzz.Fuzz(func(test *testing.T, source string) {
		if len(source) > 2048 || !utf8.ValidString(source) {
			test.Skip()
		}
		raw, err := json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": source, "opaque": "private"})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildDecodedMarkdownFixture(test, raw)
		input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		assertMappedAssemblyReplay(test, input, fixture.decoded.objects, baseline)
	})
}
