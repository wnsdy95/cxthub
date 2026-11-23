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
