package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

func TestDocFindCoordinatesIndexDifferential(test *testing.T) {
	for _, encoded := range []string{
		`""`, `"plain"`, "\"\ud55c🙂\ub05d\"",
		`"\u0000\b\f\n\r\t\"\\\/\u003c\u003E\u0026\u2028\u2029"`,
		`"\ud800"`, `"\udc00"`, `"\ud800x\udc00"`, `"\uD83D\uDE42"`,
		`"\uD83D\uD83D\uDE00"`, `"\udc00\ud800"`, `"\ud800\u0041\udc00"`,
		"\"\ud55c\\ud800🙂\\ufffd\\udc00\ub05d\"",
	} {
		raw := []byte(encoded)
		if err := validateDocDisplaySource(context.Background(), raw); err != nil {
			test.Fatal(err)
		}
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			test.Fatal(err)
		}
		index, err := newDocFindSourceStringIndex(context.Background(), raw, MaxDocFindDisplayBytes)
		if err != nil {
			test.Fatal(err)
		}
		for begin := 0; begin <= len(decoded); begin++ {
			for end := begin + 1; end <= len(decoded)+1; end++ {
				docFindCoordinatesAssertParity(test, raw, index, begin, end-begin)
			}
		}
		for _, coordinates := range [][2]int{{-1, 1}, {0, 0}, {0, -1}, {len(decoded), 1}, {int(^uint(0) >> 1), 1}, {1, int(^uint(0) >> 1)}} {
			span, err := index.sourceRange(raw, coordinates[0], coordinates[1])
			if span != (DocFindSpan{}) || !errors.Is(err, ErrValidation) {
				test.Fatal("invalid coordinates accepted", encoded, coordinates, span, err)
			}
		}
	}
}

func TestDocFindCoordinatesIndexCheckpointBoundaries(test *testing.T) {
	for prefix := docFindSourceCheckpointStride - 12; prefix <= docFindSourceCheckpointStride+12; prefix++ {
		raw := []byte(`"` + strings.Repeat("a", prefix) + strings.Repeat("\\uD83D\\uDE42\ud55c\\ud800\\u0041\\udc00🙂\\/", 20) + `"`)
		if err := validateDocDisplaySource(context.Background(), raw); err != nil {
			test.Fatal(err)
		}
		index, err := newDocFindSourceStringIndex(context.Background(), raw, MaxDocFindDisplayBytes)
		if err != nil {
			test.Fatal(err)
		}
		capacity := (len(raw)-2)/docFindSourceCheckpointStride + 1
		if cap(index) != capacity || index.retainedBytes() != capacity*int(unsafe.Sizeof(docFindSourceCheckpoint{})) {
			test.Fatal("checkpoint storage grew beyond its reservation", len(index), cap(index), capacity)
		}
		previous := 1
		for _, checkpoint := range index {
			if int(checkpoint.raw)-previous > docFindSourceCheckpointStride+11 || int(checkpoint.raw) <= previous {
				test.Fatal("unbounded or unordered checkpoint", previous, checkpoint)
			}
			quoted := append(bytes.Clone(raw[:int(checkpoint.raw)]), '"')
			var decoded string
			if err := json.Unmarshal(quoted, &decoded); err != nil || len(decoded) != int(checkpoint.decoded) {
				test.Fatal("checkpoint splits an escape, surrogate pair, or rune", checkpoint, err)
			}
			for delta := -4; delta <= 4; delta++ {
				docFindCoordinatesAssertParity(test, raw, index, int(checkpoint.decoded)+delta, 1)
				docFindCoordinatesAssertParity(test, raw, index, max(0, int(checkpoint.decoded)-7), 14+delta)
			}
			previous = int(checkpoint.raw)
		}
	}
}

func TestDocFindCoordinatesIndexExhaustiveDisplayRanges(test *testing.T) {
	units := []struct{ encoded, decoded string }{
		{`\u0061`, "a"}, {`\uD83D\uDE42`, "🙂"}, {`\ud800`, "�"},
		{"\ud55c", "\ud55c"}, {`\u0000`, "\x00"}, {`\/`, "/"}, {"Z", "Z"},
	}
	var encoded, decoded strings.Builder
	for _, unit := range units {
		encoded.WriteString(unit.encoded)
		decoded.WriteString(unit.decoded)
	}
	for _, scenario := range []struct {
		name, prefix, suffix string
		public               bool
	}{
		{"message", `{"kind":"message","blocks":[{"type":"text","text":"`, `"}]}`, false},
		{"summary", `{"kind":"message","compact_summary":true,"blocks":[{"type":"text","text":"`, `"}]}`, false},
		{"reasoning", `{"kind":"reasoning","redacted_summary":"`, `"}`, false},
		{"public reasoning", `"`, `"`, true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			count := (MaxDocMarkdownDisplayBytes - len(scenario.prefix) - len(scenario.suffix)) / encoded.Len()
			raw := []byte(scenario.prefix + strings.Repeat(encoded.String(), count) + scenario.suffix)
			before := bytes.Clone(raw)
			var display DocFindDisplayText
			var err error
			if scenario.public {
				display, err = BuildDocPublicDisplay(context.Background(), raw, EventReasoning, false)
			} else {
				display, err = BuildDocEventDisplay(context.Background(), raw)
			}
			if err != nil {
				test.Fatal(err)
			}
			if display.Text() != strings.Repeat(decoded.String(), count) || DocFindDisplayBytes(display) > MaxDocFindDisplayBytes {
				test.Fatal("display text or retention changed")
			}
			for repetition := count - 1; repetition >= 0; repetition-- {
				offset := (repetition + 1) * decoded.Len()
				position := len(scenario.prefix) + (repetition+1)*encoded.Len()
				for unitIndex := len(units) - 1; unitIndex >= 0; unitIndex-- {
					unit := units[unitIndex]
					offset -= len(unit.decoded)
					position -= len(unit.encoded)
					span, err := display.SourceRange(offset, len(unit.decoded))
					want := DocFindSpan{From: position, To: position + len(unit.encoded)}
					if err != nil || span != want {
						test.Fatal("reverse-order rune lookup differs", offset, span, want, err)
					}
				}
				span, err := display.SourceRange(offset, len(display.Text())-offset)
				want := DocFindSpan{From: position, To: len(raw) - len(scenario.suffix)}
				if err != nil || span != want {
					test.Fatal("long range endpoint lookup differs", span, want, err)
				}
			}
			matches, err := FindDocText(context.Background(), display.Text(), "Z", (count-DocFindPageMatches)*decoded.Len(), DocFindPageMatches)
			if err != nil || len(matches) != DocFindPageMatches {
				test.Fatal("late display search changed", len(matches), err)
			}
			for _, match := range matches {
				span, err := display.SourceRange(match.Offset, match.Length)
				position := len(scenario.prefix) + (match.Offset/decoded.Len()+1)*encoded.Len() - 1
				if err != nil || span != (DocFindSpan{From: position, To: position + 1}) {
					test.Fatal("late search match source changed", match, span, err)
				}
			}
			if !bytes.Equal(raw, before) {
				test.Fatal("index or lookup mutated source")
			}
		})
	}
}

func TestDocFindCoordinatesIndexMarkdownAndSummaryCover(test *testing.T) {
	for _, summary := range []bool{false, true} {
		raw := []byte(fmt.Sprintf("{\"kind\":\"message\",\"compact_summary\":%t,\"blocks\":[{\"type\":\"text\",\"text\":\"# **\\uD83D\\uDE42 label**\\n\\n[tail](https://hidden.example)\"},{\"type\":\"text\",\"text\":\"\\ud800\ud55c\"}]}", summary))
		display, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		wantText := "🙂 label\n\ntail\n\n�\ud55c"
		wantSource := "\\uD83D\\uDE42 label**\\n\\n[tail](https://hidden.example)\"},{\"type\":\"text\",\"text\":\"\\ud800\ud55c"
		if summary {
			wantText = "🙂 label\n\ntail\n�\ud55c"
		}
		span, err := display.SourceRange(0, len(display.Text()))
		if err != nil || display.Text() != wantText || string(raw[span.From:span.To]) != wantSource {
			test.Fatal("cover or block separator changed", summary, display.Text(), span, err)
		}
		for _, coordinates := range [][2]int{{0, 1}, {1, 3}, {0, 0}, {len(display.Text()), 1}} {
			span, err := display.SourceRange(coordinates[0], coordinates[1])
			if span != (DocFindSpan{}) || !errors.Is(err, ErrValidation) {
				test.Fatal("split rune or empty range accepted", coordinates, span, err)
			}
		}
	}
	for _, suffix := range []string{"", " \n\t\r"} {
		display, err := BuildDocPublicDisplay(context.Background(), []byte(`"\uD83D\uDE42"`+suffix), EventReasoning, false)
		if err != nil {
			test.Fatal("trailing JSON whitespace changed", err)
		}
		span, err := display.SourceRange(0, len(display.Text()))
		if err != nil || span != (DocFindSpan{From: 1, To: 13}) {
			test.Fatal("trailing whitespace changed coordinates", span, err)
		}
	}
}

func TestDocFindCoordinatesIndexBudgetAndSourceBounds(test *testing.T) {
	raw := []byte(`"` + strings.Repeat(`\u0061`, 4096) + `"`)
	block, err := buildDocJSONMarkdown(context.Background(), raw, 0, MaxDocFindDisplayBytes)
	if err != nil {
		test.Fatal(err)
	}
	display := block.display.(docFindJSONDisplay)
	indexBytes := cap(display.source) * int(unsafe.Sizeof(docFindSourceCheckpoint{}))
	wantBytes := int(unsafe.Sizeof(display)) + indexBytes + DocFindDisplayBytes(display.display)
	if DocFindDisplayBytes(display) != wantBytes || indexBytes == 0 {
		test.Fatal("index retention is not accounted", DocFindDisplayBytes(display), wantBytes)
	}
	for _, budget := range []int{wantBytes - indexBytes, wantBytes - 1} {
		result, err := BuildDocPublicDisplayWithin(context.Background(), raw, EventReasoning, false, budget)
		if result != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
			test.Fatal("index exceeded the display budget", budget, err)
		}
	}
	if index, err := newDocFindSourceStringIndex(context.Background(), raw, indexBytes-1); index != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal("index allocation exceeded its reservation", index, err)
	}
	bounded := []byte(`"` + strings.Repeat("a", MaxDocMarkdownDisplayBytes-2) + `"`)
	result, err := BuildDocPublicDisplay(context.Background(), bounded, EventReasoning, false)
	if err != nil || result == nil || len(result.Text()) != len(bounded)-2 || DocFindDisplayBytes(result) > MaxDocFindDisplayBytes {
		test.Fatal("128 KiB source boundary changed", err)
	}
	if result, err := BuildDocPublicDisplay(context.Background(), append(bounded, ' '), EventReasoning, false); result != nil || !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal("oversize source accepted", err)
	}
	for _, malformed := range [][]byte{[]byte{'"', 0xff, '"'}, []byte(`"\u00Q0"`), []byte(`"\ud800`), []byte(`"ok" "extra"`)} {
		if result, err := BuildDocPublicDisplay(context.Background(), malformed, EventReasoning, false); result != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid source acquired an index", malformed, err)
		}
	}
	for _, empty := range []string{`""`, `"\n\t "`} {
		result, err := BuildDocPublicDisplay(context.Background(), []byte(empty), EventReasoning, false)
		if err != nil || result == nil || result.Text() != "" {
			test.Fatal("empty display changed", empty, err)
		}
		if span, err := result.SourceRange(0, 1); span != (DocFindSpan{}) || !errors.Is(err, ErrValidation) {
			test.Fatal("empty display acquired a source range", span, err)
		}
	}
}

func TestDocFindCoordinatesIndexCancellation(test *testing.T) {
	raw := []byte(`"` + strings.Repeat(`\uD83D\uDE42`, 1024) + `"`)
	counter := &docFindCoordinatesCancelContext{Context: context.Background()}
	if index, err := newDocFindSourceStringIndex(counter, raw, MaxDocFindDisplayBytes); err != nil || index == nil {
		test.Fatal(err)
	}
	for cancelAt := 1; cancelAt <= counter.checks; cancelAt++ {
		ctx := &docFindCoordinatesCancelContext{Context: context.Background(), cancelAt: cancelAt}
		if index, err := newDocFindSourceStringIndex(ctx, raw, MaxDocFindDisplayBytes); index != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("canceled index returned partial checkpoints", cancelAt, err)
		}
	}
	counter = &docFindCoordinatesCancelContext{Context: context.Background()}
	if result, err := BuildDocPublicDisplay(counter, raw, EventReasoning, false); err != nil || result == nil {
		test.Fatal(err)
	}
	for cancelAt := 1; cancelAt <= counter.checks; cancelAt++ {
		ctx := &docFindCoordinatesCancelContext{Context: context.Background(), cancelAt: cancelAt}
		if result, err := BuildDocPublicDisplay(ctx, raw, EventReasoning, false); result != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("canceled display returned a partial index", cancelAt, err)
		}
	}
}

func docFindCoordinatesAssertParity(test *testing.T, raw []byte, index docFindSourceStringIndex, offset, length int) {
	test.Helper()
	var spans [1]DocFindSpan
	cursor := docFindSourceCursor{raw: raw, spans: spans[:]}
	wantErr := cursor.locate(DocFindSpan{From: 0, To: len(raw)}, []docFindSourceRequest{{offset: offset, length: length}})
	got, err := index.sourceRange(raw, offset, length)
	if wantErr != nil {
		if got != (DocFindSpan{}) || !errors.Is(err, wantErr) {
			test.Fatal("invalid endpoint differs from sequential mapping", offset, length, got, err, wantErr)
		}
		return
	}
	if err != nil || got != spans[0] {
		test.Fatal("indexed mapping differs from sequential mapping", offset, length, got, spans[0], err)
	}
}

type docFindCoordinatesCancelContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *docFindCoordinatesCancelContext) Err() error {
	ctx.checks++
	if ctx.cancelAt > 0 && ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
