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

func assertMappingReviewReplay(test *testing.T, input DocDisplayAssemblyInput, objects map[ContentHash][]byte, baseline DocFindDisplayText, maxBytes, maxRuns int) (int, int) {
	test.Helper()
	cursor := bytes.Clone(input.initial)
	var output strings.Builder
	seen := make(map[string]bool)
	fragments, maxLoads := 0, 0
	for output.Len() < input.source.TextBytes {
		if seen[string(cursor)] {
			test.Fatal("replay repeated a cursor", output.Len())
		}
		seen[string(cursor)] = true
		loads := 0
		load := func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			raw, found := objects[hash]
			if !found {
				return nil, ErrNotFound
			}
			return raw, nil
		}
		page, next, err := input.read(context.Background(), cursor, maxBytes, maxRuns, load)
		position, cursorErr := input.cursor(next)
		if err != nil || cursorErr != nil || page.From != output.Len() || len(page.Text) == 0 || len(page.Text) > maxBytes || len(page.Runs) > maxRuns || loads > DocDisplayMappedReplayObjects || position != output.Len()+len(page.Text) || bytes.Equal(cursor, next) {
			test.Fatalf("unbounded or stalled replay: from=%d bytes=%d runs=%d loads=%d position=%d read=%v cursor=%v", page.From, len(page.Text), len(page.Runs), loads, position, err, cursorErr)
		}
		for offset, character := range page.Text {
			width := utf8.RuneLen(character)
			for _, length := range []int{width, len(page.Text) - offset} {
				got, failure := page.SourceRange(offset, length)
				want, baselineErr := baseline.SourceRange(page.From+offset, length)
				if failure != nil || baselineErr != nil || got != want {
					test.Fatalf("original coordinates changed: from=%d offset=%d length=%d got=%v want=%v read=%v baseline=%v", page.From, offset, length, got, want, failure, baselineErr)
				}
			}
		}
		replayed, repeated, repeatErr := input.read(context.Background(), cursor, maxBytes, maxRuns, load)
		if repeatErr != nil || !reflect.DeepEqual(replayed, page) || !bytes.Equal(repeated, next) || loads > 2*DocDisplayMappedReplayObjects {
			test.Fatal("identical cursor did not replay deterministically", repeatErr, loads)
		}
		maxLoads = max(maxLoads, loads/2)
		fragments++
		output.WriteString(page.Text)
		cursor = next
	}
	if output.String() != baseline.Text() {
		test.Fatal("replayed text changed", output.String(), baseline.Text())
	}
	return fragments, maxLoads
}

func TestDocDisplayMappingReviewEscapesSurrogatesAndSmallLimits(test *testing.T) {
	for _, token := range []string{
		`"**\u0061\uD83D\uDE42\uD800\uDC00\uD800x\uDC00** \\\"\/\b\f\tend"`,
		`"a` + strings.Repeat(`\t`, 1700) + `\u0062\n\n\uD83D\uDE42"`,
		`"` + strings.Repeat(`\u0061`, DocDisplayPageRuns-1) + `\uD83D\uDE42\uD800 **tail**"`,
	} {
		raw := []byte(`{"kind":"reasoning","redacted_summary":` + token + `}`)
		fixture := buildDecodedMarkdownFixture(test, raw)
		input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		for _, limits := range [][2]int{{4, 1}, {37, 7}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
			assertMappingReviewReplay(test, input, fixture.decoded.objects, baseline, limits[0], limits[1])
		}
	}
}

func TestDocDisplayMappingReviewSparseInputYieldsAndResumes(test *testing.T) {
	token := `"` + strings.Repeat(`[\u0061](https://hidden/`+strings.Repeat(`\u0062`, 700)+`) `, 27) + `end"`
	raw := []byte(`{"kind":"reasoning","redacted_summary":` + token + `}`)
	fixture := buildDecodedMarkdownFixture(test, raw)
	input, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	fragments, loads := assertMappingReviewReplay(test, input, fixture.decoded.objects, baseline, DocDisplayPageTextBytes, DocDisplayPageRuns)
	if fragments < 2 || loads != DocDisplayMappedReplayObjects {
		test.Fatal("sparse fixture did not exercise an object-budget yield", fragments, loads)
	}
	test.Logf("event_bytes=%d displayed_bytes=%d fragments=%d max_object_loads=%d", len(raw), input.source.TextBytes, fragments, loads)
}

func TestDocDisplayMappingReviewSummarySyntheticNewlinesAndSparseAnchors(test *testing.T) {
	for _, blocks := range []string{
		`[{"text":"**first"},` + strings.Repeat(`{"opaque":"`+strings.Repeat("private", 4)+`"},`, 1700) + `{"text":"last**"}]`,
		`[{"text":"a\uD83D\uDE42"},{},{"text":null},{"text":""},{"text":"\uD800\r\n**last**"}]`,
		`[{"text":"` + strings.Repeat(`\u0061`, DocDisplayPageRuns-1) + `"},{},{"text":"\uD83D\uDE42"}]`,
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
		for _, limits := range [][2]int{{4, 1}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
			assertMappingReviewReplay(test, input, fixture.input.objects, baseline, limits[0], limits[1])
		}
	}
}

func TestDocDisplayMappingReviewPlaceholderLimitsAndEscapedPageEdges(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"` + strings.Repeat(`\u0061`, 2100) + `\uD83D\uDE42","name":"` + strings.Repeat(`\u0062`, 2200) + `\uD800","text":"private"}]}`)
	plan, objects := messagePlaceholderFixture(test, raw)
	input, err := NewDocMessagePlaceholderAssemblyInput(plan)
	if err != nil {
		test.Fatal(err)
	}
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	assertMappingReviewReplay(test, input, objects, baseline, 511, 1)
	assertMappingReviewReplay(test, input, objects, baseline, DocDisplayPageTextBytes, DocDisplayPageRuns)
}

func TestDocDisplayMappingReviewCursorCanonicalityAndSourceIsolation(test *testing.T) {
	first := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**same**","opaque":"first"}`))
	second := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","opaque":"other source","redacted_summary":"**same**"}`))
	left, err := NewDocDecodedMarkdownAssemblyInput(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	right, err := NewDocDecodedMarkdownAssemblyInput(second.plan)
	if err != nil {
		test.Fatal(err)
	}
	var native docMappedMarkdownCursor
	if err := json.Unmarshal(left.initial, &native); err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []json.RawMessage{
		append(bytes.Clone(left.initial), ' '),
		append([]byte(`{"version":1,`), left.initial[1:]...),
		append([]byte(`{"unknown":null,`), left.initial[1:]...),
		bytes.Replace(left.initial, []byte(`"version":1`), []byte(`"version":1.0`), 1),
		right.initial,
	} {
		calls := 0
		page, next, failure := left.read(context.Background(), candidate, 64, 8, func(context.Context, ContentHash) ([]byte, error) {
			calls++
			return nil, ErrNotFound
		})
		if !errors.Is(failure, ErrIntegrity) || calls != 0 || next != nil || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("foreign or noncanonical cursor reached prepared objects", calls, failure)
		}
	}
	native.Render.Plan = right.source.Plan
	changed, err := canonicalJSON(native)
	if err != nil {
		test.Fatal(err)
	}
	if _, failure := left.cursor(changed); !errors.Is(failure, ErrIntegrity) {
		test.Fatal("outer source binding hid a foreign render plan", failure)
	}
}

func TestDocDisplayMappingReviewLiteralGapRatioRemainsStrict(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 513*4))
	reader := fixture.reader(test)
	from, length := 1, len(fixture.text)-2
	span, err := reader.locateSource(context.Background(), from, length, false)
	if err != nil || span != (DocFindSpan{From: 1, To: 300512}) {
		test.Fatal("sparse nonliteral range was not supported", span, err)
	}
	if span, err := fixture.reader(test).locateSource(context.Background(), from, length, true); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
		test.Fatal("sparse raw gap weakened the literal byte-ratio guard", span, err)
	}
	for _, sourceFrom := range []int{1538, 6670} {
		for index := range fixture.pages[3].Runs {
			fixture.pages[3].Runs[index].SourceFrom = sourceFrom
			fixture.pages[3].Runs[index].SourceTo = sourceFrom + len(fixture.pages[3].Text)
		}
		if span, err := fixture.reader(test).locateSource(context.Background(), from, length, true); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
			test.Fatal("impossible literal gap ratio was accepted", sourceFrom, span, err)
		}
	}
}
