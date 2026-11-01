package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocMarkdownDisplaySharedCases(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/markdown-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Version int                                   `json:"version"`
		Cases   []struct{ Name, Source, Text string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Version != DocMarkdownDisplayVersion || len(fixture.Cases) < 30 {
		test.Fatal("invalid shared display fixture", err, fixture.Version)
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			display, err := BuildDocMarkdownDisplay(context.Background(), scenario.Source)
			if err != nil || display.Text() != scenario.Text {
				test.Fatalf("display = %q, %v; want %q", display.Text(), err, scenario.Text)
			}
			assertDocMarkdownDisplayRanges(test, scenario.Source, display)
		})
	}
}

func assertDocMarkdownDisplayRanges(test *testing.T, source string, display DocMarkdownDisplay) {
	test.Helper()
	previous := 0
	for index := 0; index < display.spanCount; index++ {
		span := display.span(index)
		if span.from != previous || span.to <= span.from || span.sourceFrom < 0 || span.sourceTo < span.sourceFrom || span.sourceTo > len(source) {
			test.Fatal("invalid provenance range", span, previous)
		}
		previous = span.to
	}
	if previous != len(display.Text()) {
		test.Fatal("display provenance has a gap", previous, len(display.Text()))
	}
	for offset, character := range display.Text() {
		span, err := display.SourceRange(offset, utf8.RuneLen(character))
		if err != nil || span.From < 0 || span.From >= span.To || span.To > len(source) || !utf8.ValidString(source[span.From:span.To]) {
			test.Fatal("display rune has no valid source range", offset, span, err)
		}
	}
}

func TestDocMarkdownDisplaySearchAndCanonicalSourceComposition(test *testing.T) {
	for _, scenario := range []struct{ source, query, original string }{
		{"before **bold** after", "before bold after", "before **bold** after"},
		{"# Alpha  **beta**\t end", "Alpha beta end", "Alpha  **beta**\t end"},
		{"한글 **🙂 끝**", "🙂 끝", "🙂 끝"},
		{"before [label](https://hidden.example/needle) after", "before label after", "before [label](https://hidden.example/needle) after"},
		{"first\n# second", "first\n\nsecond", "first\n# second"},
	} {
		display, err := BuildDocMarkdownDisplay(context.Background(), scenario.source)
		if err != nil {
			test.Fatal(err)
		}
		matches, err := FindDocText(context.Background(), display.Text(), scenario.query, 0, DocFindPageMatches)
		if err != nil || len(matches) != 1 {
			test.Fatal("visible query missing", scenario, display.Text(), matches, err)
		}
		span, err := display.SourceRange(matches[0].Offset, matches[0].Length)
		if err != nil || scenario.source[span.From:span.To] != scenario.original {
			test.Fatal("source provenance lost", scenario, span, err)
		}
		event, err := json.Marshal(CIREvent{Kind: EventMessage, Blocks: []ContentBlock{{Type: "text", Text: scenario.source}}})
		if err != nil {
			test.Fatal(err)
		}
		before := bytes.Clone(event)
		physical, err := LocateDocFindTextSource(event, 0, span.From, span.To-span.From)
		if err != nil || physical.From >= physical.To || physical.To > len(event) || !bytes.Equal(before, event) {
			test.Fatal("projection cannot compose with unchanged canonical JSON", physical, err)
		}
		if strings.Contains(scenario.source, "hidden.example") {
			matches, err = FindDocText(context.Background(), display.Text(), "hidden.example", 0, DocFindPageMatches)
			if err != nil || len(matches) != 0 {
				test.Fatal("invisible link target was searched", matches, err)
			}
		}
	}
}

func TestDocMarkdownDisplayLimitsCancellationAndUTF8(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if display, err := BuildDocMarkdownDisplay(ctx, "text"); !errors.Is(err, context.Canceled) || display.Text() != "" {
		test.Fatal("cancelled projection returned text", err)
	}
	if display, err := BuildDocMarkdownDisplay(context.Background(), strings.Repeat("x", MaxDocMarkdownDisplayBytes+1)); !errors.Is(err, ErrDocMarkdownDisplayLimit) || display.Text() != "" {
		test.Fatal("source limit bypassed", err)
	}
	for _, source := range []string{string([]byte{0xff}), "a\xf0\x9f", "a\xc0\xaf"} {
		if display, err := BuildDocMarkdownDisplay(context.Background(), source); !errors.Is(err, ErrIntegrity) || display.Text() != "" {
			test.Fatal("invalid UTF-8 accepted", err)
		}
	}
	display, err := BuildDocMarkdownDisplay(context.Background(), "🙂 한글")
	if err != nil {
		test.Fatal(err)
	}
	for _, query := range [][2]int{{-1, 1}, {0, 0}, {0, -1}, {1, 2}, {0, 3}, {5, 1}, {len(display.Text()), 1}, {int(^uint(0) >> 1), 2}} {
		if span, err := display.SourceRange(query[0], query[1]); !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			test.Fatal("invalid match accepted", query, span, err)
		}
	}
}

func TestDocMarkdownDisplayMalformedAndDenseInputsRemainBounded(test *testing.T) {
	for _, source := range []string{
		strings.Repeat("[", MaxDocMarkdownDisplayBytes), strings.Repeat("`", MaxDocMarkdownDisplayBytes),
		strings.Repeat("*", MaxDocMarkdownDisplayBytes), strings.Repeat("# malformed\u2028heading\n", 5000),
		strings.Repeat("a**b**", MaxDocMarkdownDisplayBytes/6),
	} {
		display, err := BuildDocMarkdownDisplay(context.Background(), source)
		if err != nil || len(display.Text()) > len(source) || display.spanCount > len(source) {
			test.Fatal("unbounded projection", len(source), len(display.Text()), display.spanCount, err)
		}
		if display.Text() != "" {
			if _, err := display.SourceRange(0, len(display.Text())); err != nil {
				test.Fatal("full display source missing", err)
			}
		}
	}
}

func FuzzDocMarkdownDisplay(fuzz *testing.F) {
	for _, source := range []string{"# heading", "**bold**", "before [label](https://host/path) after", "# bad\u2028heading", "- a\r\n- b", "```\n\r\n```", "", "🙂\n\n한"} {
		fuzz.Add(source)
	}
	fuzz.Fuzz(func(test *testing.T, source string) {
		display, err := BuildDocMarkdownDisplay(context.Background(), source)
		if err != nil {
			return
		}
		if !utf8.ValidString(display.Text()) || len(display.Text()) > len(source) || display.spanCount > len(source) {
			test.Fatal("invalid display bounds")
		}
		assertDocMarkdownDisplayRanges(test, source, display)
	})
}

func BenchmarkDocMarkdownDisplay(benchmark *testing.B) {
	for _, scenario := range []struct{ name, source string }{
		{"literal128KiB", strings.Repeat("a", MaxDocMarkdownDisplayBytes)},
		{"unmatchedBrackets128KiB", strings.Repeat("[", MaxDocMarkdownDisplayBytes)},
		{"denseInline128KiB", strings.Repeat("a**b**", MaxDocMarkdownDisplayBytes/6)},
		{"normalWhitespace128KiB", "# " + strings.Repeat("a \t", (MaxDocMarkdownDisplayBytes-2)/3)},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			benchmark.SetBytes(int64(len(scenario.source)))
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				if _, err := BuildDocMarkdownDisplay(context.Background(), scenario.source); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}

func TestDocMarkdownDisplayAllocationBounds(test *testing.T) {
	for _, scenario := range []struct {
		name, source string
		limit        uint64
	}{
		{"literal", strings.Repeat("a", MaxDocMarkdownDisplayBytes), 256 << 10},
		{"denseInline", strings.Repeat("a**b**", MaxDocMarkdownDisplayBytes/6), 3 << 20},
		{"collapsedWhitespace", "# " + strings.Repeat("a \t", (MaxDocMarkdownDisplayBytes-2)/3), 4 << 20},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for iteration := 0; iteration < 2; iteration++ {
				if _, err := BuildDocMarkdownDisplay(context.Background(), scenario.source); err != nil {
					test.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for iteration := 0; iteration < 5; iteration++ {
				if _, err := BuildDocMarkdownDisplay(context.Background(), scenario.source); err != nil {
					test.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			allocated := (after.TotalAlloc - before.TotalAlloc) / 5
			if allocated > scenario.limit {
				test.Fatal("projection metadata allocation exceeded bounded fixture", allocated, scenario.limit)
			}
			test.Logf("source=%d allocated_bytes=%d", len(scenario.source), allocated)
		})
	}
}

func TestDocMarkdownDisplayLinearInlineMatchesRegularGrammar(test *testing.T) {
	reference := regexp.MustCompile("(\\*\\*[^*]+\\*\\*|`[^`]+`|\\[[^\\]]+\\]\\(https?://[^)\\t\\n\\x0b\\f\\r \\x{00a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]+\\))")
	atoms := []string{"a", "*", "**", "`", "[", "](", "https://x)", "http://x)", "🙂", "\u00a0", ")", " "}
	random := rand.New(rand.NewSource(41))
	for sample := 0; sample < 2000; sample++ {
		var input strings.Builder
		input.WriteString("prefix ")
		for count := 0; count < 20; count++ {
			input.WriteString(atoms[random.Intn(len(atoms))])
		}
		source, last := input.String(), 0
		var expected strings.Builder
		for _, match := range reference.FindAllStringIndex(source, -1) {
			expected.WriteString(source[last:match[0]])
			token := source[match[0]:match[1]]
			if strings.HasPrefix(token, "**") {
				expected.WriteString(token[2 : len(token)-2])
			} else if token[0] == '`' {
				expected.WriteString(token[1 : len(token)-1])
			} else {
				expected.WriteString(token[1:strings.Index(token, "](")])
			}
			last = match[1]
		}
		expected.WriteString(source[last:])
		display, err := BuildDocMarkdownDisplay(context.Background(), source)
		if err != nil || display.Text() != expected.String() {
			test.Fatalf("inline grammar changed for %q: got %q, %v; want %q", source, display.Text(), err, expected.String())
		}
		assertDocMarkdownDisplayRanges(test, source, display)
	}
}
