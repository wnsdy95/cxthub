package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocEventDisplaySharedCases(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/event-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Version int
		Cases   []struct{ Name, Source, Text string }
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.Version != 1 || len(fixture.Cases) != 12 {
		test.Fatal("invalid fixture")
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			raw := []byte(scenario.Source)
			before := bytes.Clone(raw)
			display, err := BuildDocEventDisplay(context.Background(), raw)
			if err != nil || display.Text() != scenario.Text {
				test.Fatalf("got %q, %v; want %q", display.Text(), err, scenario.Text)
			}
			for offset, character := range display.Text() {
				span, err := display.SourceRange(offset, utf8.RuneLen(character))
				if err != nil || span.From < 0 || span.From >= span.To || span.To > len(raw) || !utf8.Valid(raw[span.From:span.To]) {
					test.Fatal("invalid source span", offset, span, err)
				}
			}
			if !bytes.Equal(before, raw) {
				test.Fatal("mutated original event")
			}
		})
	}
}

func TestDocEventDisplayCrossBlockSourceCover(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"**left**"},{"type":"text","text":"[right](https://hidden.example)"}]}`)
	display, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	span, err := display.SourceRange(0, len(display.Text()))
	if err != nil || string(raw[span.From:span.To]) != `left**"},{"type":"text","text":"[right` {
		test.Fatal(span, err, string(raw[span.From:span.To]))
	}
	for _, query := range []string{"hidden.example", "**", "text", "message"} {
		matches, err := FindDocText(context.Background(), display.Text(), query, 0, 20)
		if err != nil || len(matches) != 0 {
			test.Fatal("formatting or metadata leaked", query, matches, err)
		}
	}
}

func TestDocEventDisplayInvalidAndCanceledReturnNoPartial(test *testing.T) {
	for _, raw := range []string{`null`, `{"kind":"message","blocks":[null]}`, `{"kind":"message","blocks":[{"type":"text","text":7}]}`, `{"kind":"message","blocks":{}}`, `{`} {
		display, err := BuildDocEventDisplay(context.Background(), []byte(raw))
		if err == nil || display != nil {
			test.Fatal("invalid source accepted", raw, display, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	display, err := BuildDocEventDisplay(ctx, []byte(`{"kind":"message"}`))
	if display != nil || !errors.Is(err, context.Canceled) {
		test.Fatal(display, err)
	}
	display, err = BuildDocEventDisplay(context.Background(), []byte(strings.Repeat(" ", MaxDocMarkdownDisplayBytes+1)))
	if display != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal(display, err)
	}
}

func TestDocEventDisplayCoordinatesRejectSplitRunes(test *testing.T) {
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"type":"text","text":"한글🙂"}]}`)
	display, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, coordinates := range [][2]int{{-1, 1}, {0, 0}, {1, 2}, {0, 1}, {0, 99}} {
		span, err := display.SourceRange(coordinates[0], coordinates[1])
		if !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			test.Fatal(coordinates, span, err)
		}
	}
	span, err := display.SourceRange(0, len(display.Text()))
	if err != nil || string(raw[span.From:span.To]) != "한글🙂" {
		test.Fatal(span, err)
	}
}

func TestDocEventDisplayAggregateProvenanceAndConstructionBudget(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.TrimSuffix(strings.Repeat(`{"type":"text","text":"a"},`, 1000), ",") + `]}`)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	display, err := BuildDocEventDisplay(context.Background(), raw)
	runtime.ReadMemStats(&after)
	if err != nil || display == nil {
		test.Fatal("many small blocks rejected", err)
	}
	if len(display.Text()) != 2998 || DocFindDisplayBytes(display) > MaxDocFindDisplayBytes || after.TotalAlloc-before.TotalAlloc > 4<<20 {
		test.Fatal("aggregate provenance overallocated", DocFindDisplayBytes(display), after.TotalAlloc-before.TotalAlloc)
	}
	for _, budget := range []int{1, 100, 4096} {
		result, err := BuildDocEventDisplayWithin(context.Background(), raw, budget)
		if result != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
			test.Fatal("construction exceeded remaining budget", budget, err)
		}
	}
	dense := []byte(`{"kind":"message","blocks":[{"type":"text","text":"` + strings.Repeat("a**b**", 20_000) + `"}]}`)
	if result, err := BuildDocEventDisplay(context.Background(), dense); result != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal("dense provenance ignored budget", err)
	}
	test.Logf("source_bytes=%d display_bytes=%d retained_bytes=%d allocated_bytes=%d", len(raw), len(display.Text()), DocFindDisplayBytes(display), after.TotalAlloc-before.TotalAlloc)
}

func FuzzDocEventDisplay(fuzz *testing.F) {
	for _, raw := range []string{`null`, `{"kind":"message","blocks":[{"type":"text","text":"**한글🙂**"}]}`, `{"kind":"tool_call","tool_name":"Bash","input":{"command":"echo test"}}`, `{"kind":"tool_result","output":{"10":1,"2":"**label**"}}`} {
		fuzz.Add(raw)
	}
	fuzz.Fuzz(func(test *testing.T, source string) {
		display, err := BuildDocEventDisplay(context.Background(), []byte(source))
		if err != nil {
			if display != nil {
				test.Fatal("partial failed projection")
			}
			return
		}
		if display == nil || !utf8.ValidString(display.Text()) || DocFindDisplayBytes(display) > MaxDocFindDisplayBytes {
			test.Fatal("invalid successful projection")
		}
		if display.Text() != "" {
			span, err := display.SourceRange(0, len(display.Text()))
			if err != nil || span.From < 0 || span.From >= span.To || span.To > len(source) {
				test.Fatal(span, err)
			}
		}
	})
}
