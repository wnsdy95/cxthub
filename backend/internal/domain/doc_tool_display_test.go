package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"
)

func TestDocToolDisplaySharedCases(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/tool-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Version int `json:"version"`
		Cases   []struct {
			Name, Source, Text string
			Ranges             []struct{ Text, Source string }
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Version != DocToolDisplayVersion || len(fixture.Cases) < 25 {
		test.Fatal("invalid shared tool display fixture", err, fixture.Version)
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			source := []byte(scenario.Source)
			before := bytes.Clone(source)
			display, err := BuildDocToolDisplay(context.Background(), source)
			if err != nil || display == nil || display.Text() != scenario.Text {
				test.Fatalf("display = %v, %v; want %q", display, err, scenario.Text)
			}
			if !bytes.Equal(source, before) {
				test.Fatal("projection mutated canonical event bytes")
			}
			assertDocToolDisplayRanges(test, source, display)
			for _, expected := range scenario.Ranges {
				offset := strings.Index(display.Text(), expected.Text)
				if offset < 0 {
					test.Fatal("fixture query missing", expected.Text)
				}
				span, err := display.SourceRange(offset, len(expected.Text))
				if err != nil || string(source[span.From:span.To]) != expected.Source {
					test.Fatalf("query %q source = %q, %v; want %q", expected.Text, source[span.From:span.To], err, expected.Source)
				}
			}
		})
	}
}

func assertDocToolDisplayRanges(test *testing.T, raw []byte, display DocFindDisplayText) {
	test.Helper()
	concrete, ok := display.(*docToolDisplay)
	if !ok {
		test.Fatal("unexpected display implementation")
	}
	previous := 0
	for index := 0; index < concrete.spanCount; index++ {
		span := concrete.span(index)
		sourceTo := int(span.sourceTo &^ docToolDisplayLinear)
		if int(span.end) <= previous || int(span.sourceFrom) >= sourceTo || sourceTo > len(raw) {
			test.Fatal("invalid provenance run", span, previous)
		}
		if span.sourceTo&docToolDisplayLinear != 0 && display.Text()[previous:span.end] != string(raw[span.sourceFrom:sourceTo]) {
			test.Fatal("linear provenance does not preserve original bytes", span)
		}
		previous = int(span.end)
	}
	if previous != len(display.Text()) {
		test.Fatal("provenance gap", previous, len(display.Text()))
	}
	if concrete.retainedBytes() > MaxDocToolDisplayBytes {
		test.Fatal("unbounded text plus provenance", concrete.retainedBytes())
	}
	if display.Text() == "" {
		if span, err := display.SourceRange(0, 1); !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			test.Fatal("empty display returned provenance", span, err)
		}
		return
	}
	from, to := len(raw), 0
	for offset, character := range display.Text() {
		span, err := display.SourceRange(offset, utf8.RuneLen(character))
		if err != nil || span.From < 0 || span.From >= span.To || span.To > len(raw) || !utf8.Valid(raw[span.From:span.To]) {
			test.Fatal("display rune lost original UTF8 provenance", offset, span, err)
		}
		from, to = min(from, span.From), max(to, span.To)
		prefix, err := display.SourceRange(0, offset+utf8.RuneLen(character))
		if err != nil || prefix != (DocFindSpan{From: from, To: to}) {
			test.Fatal("range did not cover the min/max of all participating source spans", prefix, from, to, err)
		}
	}
}

func docToolDisplayTestRaw(test testing.TB, event CIREvent) []byte {
	test.Helper()
	raw, err := canonicalJSON(event)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func TestDocToolDisplayInvalidSourceAndKind(test *testing.T) {
	invalid := [][]byte{
		nil, []byte(`{`), []byte(`{} {}`), []byte(`[]`), []byte(`null`), []byte(`"tool_result"`),
		[]byte(`{}`), []byte(`{"kind":1}`), []byte(`{"kind":null}`),
		[]byte(`{"kind":"tool_call","input":[]}`), []byte(`{"kind":"tool_call","input":"x"}`),
		[]byte(`{"kind":"tool_call","tool_name":17}`), []byte(`{"kind":"tool_call","provider_tool_name":false}`),
		[]byte(`{"kind":"tool_result","output":"\q"}`),
		[]byte(`{"kind":"tool_result","output":01}`),
		append(append([]byte(`{"kind":"tool_result","output":"`), 0xff), []byte(`"}`)...),
		[]byte(strings.Repeat(" ", maxDocFindSourceBytes+1)),
	}
	for position, raw := range invalid {
		display, err := BuildDocToolDisplay(context.Background(), raw)
		if !errors.Is(err, ErrIntegrity) || display != nil {
			test.Fatal("invalid source accepted", position, display, err)
		}
	}
	for _, kind := range []string{"message", "reasoning", "compaction", "turn", "unknown", ""} {
		display, err := BuildDocToolDisplay(context.Background(), []byte(fmt.Sprintf(`{"kind":%q,"locked":{"blob":"never searchable"}}`, kind)))
		if !errors.Is(err, ErrValidation) || display != nil {
			test.Fatal("wrong kind accepted", kind, display, err)
		}
	}
}

func TestDocToolDisplayInvalidMatchBoundaries(test *testing.T) {
	raw := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: "🙂 한글"})
	display, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, request := range [][2]int{{-1, 1}, {0, 0}, {0, -1}, {1, 2}, {0, 3}, {5, 1}, {len(display.Text()), 1}, {math.MaxInt, 2}, {1, math.MaxInt}} {
		if span, err := display.SourceRange(request[0], request[1]); !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			test.Fatal("invalid match accepted", request, span, err)
		}
	}
}

func TestDocToolDisplayOriginalBytesAndReordering(test *testing.T) {
	raw := []byte(" \n" + `{"call_id":"c","input":{"file_path":"/a/b/c/d","new_string":"new\nend","old_string":"\u006fld\r\nstart"},"kind":"tool_call","seq":0,"tool_name":"Edit"}` + "\t")
	display, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	assertDocToolDisplayRanges(test, raw, display)
	query := "old\r\n− start\n+ new"
	offset := strings.Index(display.Text(), query)
	span, err := display.SourceRange(offset, len(query))
	if err != nil || string(raw[span.From:span.To]) != `"new\nend","old_string":"\u006fld\r\nstart` {
		test.Fatal("diff source was treated as monotonic or reserialized", span, err)
	}
	before := display.Text()
	for position := range raw {
		raw[position] = 'x'
	}
	if display.Text() != before {
		test.Fatal("display retained mutable input bytes")
	}
}

func TestDocToolDisplayBeyondAllUIClippingLimits(test *testing.T) {
	lines := strings.Repeat("**literal**\r\n", 120) + "tail needle"
	for _, scenario := range []struct {
		name     string
		event    CIREvent
		expected string
	}{
		{"Write", CIREvent{Kind: EventToolCall, ToolName: "Write", Input: map[string]any{"content": lines}}, "Write\n+ " + strings.ReplaceAll(lines, "\n", "\n+ ")},
		{"Edit", CIREvent{Kind: EventToolCall, ToolName: "Edit", Input: map[string]any{"old_string": lines, "new_string": lines}}, "Edit\n− " + strings.ReplaceAll(lines, "\n", "\n− ") + "\n+ " + strings.ReplaceAll(lines, "\n", "\n+ ")},
		{"Bash", CIREvent{Kind: EventToolCall, ToolName: "Bash", Input: map[string]any{"command": strings.Repeat("x", 9000) + "tail needle"}}, "Bash(" + strings.Repeat("x", 9000) + "tail needle)\n$ " + strings.Repeat("x", 9000) + "tail needle"},
		{"generic", CIREvent{Kind: EventToolCall, ProviderToolName: "functions.exec", Input: map[string]any{"cmd": strings.Repeat("x", 5000) + "tail needle"}}, "functions.exec\n{\n  \"cmd\": \"" + strings.Repeat("x", 5000) + "tail needle\"\n}"},
		{"result", CIREvent{Kind: EventToolResult, Output: strings.Repeat("x", 12000) + "tail needle"}, strings.Repeat("x", 12000) + "tail needle"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := docToolDisplayTestRaw(test, scenario.event)
			display, err := BuildDocToolDisplay(context.Background(), raw)
			if err != nil || display == nil || display.Text() != scenario.expected {
				test.Fatal("logical expanded content was clipped or normalized", err)
			}
			offset := strings.LastIndex(display.Text(), "tail needle")
			span, err := display.SourceRange(offset, len("tail needle"))
			if err != nil || string(raw[span.From:span.To]) != "tail needle" {
				test.Fatal("unclipped tail lost source provenance", span, err)
			}
		})
	}
}

func TestDocToolDisplayBoundsAndNoPartialFallback(test *testing.T) {
	empty := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: ""})
	limitEvent := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: strings.Repeat("x", maxDocFindSourceBytes-len(empty))})
	if len(limitEvent) != maxDocFindSourceBytes {
		test.Fatal("invalid exact-limit fixture", len(limitEvent))
	}
	display, err := BuildDocToolDisplay(context.Background(), limitEvent)
	if err != nil || display == nil || len(display.Text()) != maxDocFindSourceBytes-len(empty) {
		test.Fatal("exact source limit rejected", err)
	}
	if display, err := BuildDocToolDisplay(context.Background(), append(limitEvent, ' ')); !errors.Is(err, ErrIntegrity) || display != nil {
		test.Fatal("source limit bypassed", err)
	}
	for _, depth := range []int{64, 300} {
		raw := []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`)
		display, err := BuildDocToolDisplay(context.Background(), raw)
		if err != nil || display == nil || strings.Count(display.Text(), "[") != depth {
			test.Fatal("bounded nested content rejected", depth, err)
		}
	}
	expanding := [][]byte{
		[]byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 900) + `0` + strings.Repeat("]", 900) + `}`),
		docToolDisplayTestRaw(test, CIREvent{Kind: EventToolCall, ToolName: "Write", Input: map[string]any{"content": strings.Repeat("\n", 40000)}}),
		[]byte(`{"kind":"tool_result","output":[` + strings.Repeat("0,", 20000) + `0]}`),
	}
	for position, raw := range expanding {
		if len(raw) > maxDocFindSourceBytes {
			test.Fatal("expansion test exceeds source limit", position)
		}
		display, err := BuildDocToolDisplay(context.Background(), raw)
		if !errors.Is(err, ErrDocFindDisplayUnavailable) || display != nil {
			test.Fatal("expanded text or metadata limit did not fail closed", position, display, err)
		}
	}
}

func TestDocToolDisplayPrivateBudget(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		output string
	}{
		{"empty", ""},
		{"plain", "visible"},
		{"multiple pages and spare outer capacity", strings.Repeat("<\n", 3000)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: scenario.output})
			original, err := BuildDocToolDisplay(context.Background(), raw)
			if err != nil {
				test.Fatal(err)
			}
			concrete := original.(*docToolDisplay)
			retained := int(unsafe.Sizeof(*concrete)) + len(concrete.text) + cap(concrete.spans)*int(unsafe.Sizeof([]docToolDisplaySpan{}))
			for _, page := range concrete.spans {
				retained += cap(page) * int(unsafe.Sizeof(docToolDisplaySpan{}))
			}
			if concrete.retainedBytes() != retained {
				test.Fatal("retained accounting omitted allocated capacity or fixed overhead", concrete.retainedBytes(), retained)
			}
			if scenario.output == "" && retained != docToolDisplayBaseBytes {
				test.Fatal("empty display must reserve only the fixed display overhead", retained)
			}
			if scenario.name == "multiple pages and spare outer capacity" && cap(concrete.spans) <= len(concrete.spans) {
				test.Fatal("fixture must exercise spare outer slice capacity", len(concrete.spans), cap(concrete.spans))
			}
			for _, budget := range []int{math.MinInt, -1, 0, docToolDisplayBaseBytes - 1, len(original.Text()), retained - 1} {
				display, err := buildDocToolDisplay(context.Background(), raw, budget)
				if !errors.Is(err, ErrDocFindDisplayUnavailable) || display != nil {
					test.Fatal("insufficient private budget returned partial display", budget, retained, display, err)
				}
			}
			for _, budget := range []int{retained, retained + 1, MaxDocToolDisplayBytes - 1, MaxDocToolDisplayBytes, MaxDocToolDisplayBytes + 1, math.MaxInt} {
				display, err := buildDocToolDisplay(context.Background(), raw, budget)
				if err != nil || display == nil || display.Text() != original.Text() || display.(*docToolDisplay).retainedBytes() > min(budget, MaxDocToolDisplayBytes) {
					test.Fatal("sufficient private budget changed the default projection", budget, retained, display, err)
				}
			}
		})
	}
}

func TestDocToolDisplayPrivateBudgetCapsExpansion(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 200) + `0` + strings.Repeat("]", 200) + `}`)
	display, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil || display == nil {
		test.Fatal("default projection unexpectedly changed", err)
	}
	if display, err := buildDocToolDisplay(context.Background(), raw, 32<<10); !errors.Is(err, ErrDocFindDisplayUnavailable) || display != nil {
		test.Fatal("reduced construction budget allowed expanded text", display, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if display, err := buildDocToolDisplay(ctx, raw, 0); !errors.Is(err, context.Canceled) || display != nil {
		test.Fatal("budget failure masked cancellation", display, err)
	}
	ctx = &docToolDisplayCancelContext{Context: context.Background(), remaining: 5}
	if display, err := buildDocToolDisplay(ctx, raw, 32<<10); !errors.Is(err, context.Canceled) || display != nil {
		test.Fatal("budgeted construction leaked a canceled partial display", display, err)
	}
}

type docToolDisplayCancelContext struct {
	context.Context
	remaining int
}

func (ctx *docToolDisplayCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestDocToolDisplayCancellationHasZeroPartialResult(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if display, err := BuildDocToolDisplay(ctx, []byte(`{}`)); !errors.Is(err, context.Canceled) || display != nil {
		test.Fatal("already canceled projection returned a result", display, err)
	}
	raw := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolCall, ToolName: "Write", Input: map[string]any{"content": strings.Repeat("line\n", 1000)}})
	for _, remaining := range []int{2, 5, 30, 1000} {
		ctx := &docToolDisplayCancelContext{Context: context.Background(), remaining: remaining}
		if display, err := BuildDocToolDisplay(ctx, raw); !errors.Is(err, context.Canceled) || display != nil {
			test.Fatal("mid-projection cancellation leaked text", remaining, display, err)
		}
	}
}

func TestDocToolDisplayHiddenMetadataInvariance(test *testing.T) {
	base := CIREvent{Kind: EventToolCall, ToolName: "Write", Input: map[string]any{"content": "visible\r\n**unchanged**", "file_path": "/one/two/three/four"}}
	raw := docToolDisplayTestRaw(test, base)
	original, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	base.Seq, base.ID, base.CallID, base.Status = 777, "invisible identity", "invisible call", "invisible status"
	base.ProviderMetadata = &ProviderMetadata{}
	changed := docToolDisplayTestRaw(test, base)
	changed = bytes.Replace(changed, []byte(`"kind":"tool_call"`), []byte(`"kind":"tool_call","locked":{"blob":"private locked data"}`), 1)
	display, err := BuildDocToolDisplay(context.Background(), changed)
	if err != nil || display == nil || display.Text() != original.Text() {
		test.Fatal("hidden metadata affected tool presentation", err)
	}
	assertDocToolDisplayRanges(test, changed, display)
	if strings.Contains(display.Text(), "invisible") || strings.Contains(display.Text(), "private locked data") {
		test.Fatal("hidden event metadata became searchable")
	}
}

func TestDocToolDisplayAllocationRegression(test *testing.T) {
	raw := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: strings.Repeat("x", 120<<10)})
	var failure error
	allocations := testing.AllocsPerRun(8, func() {
		_, failure = BuildDocToolDisplay(context.Background(), raw)
	})
	if failure != nil || allocations > 64 {
		test.Fatal("plain PRE content acquired per-character allocations", allocations, failure)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	display, err := BuildDocToolDisplay(context.Background(), raw)
	runtime.ReadMemStats(&after)
	if err != nil || after.TotalAlloc-before.TotalAlloc > 2<<20 {
		test.Fatal("bounded plain display allocated too much", after.TotalAlloc-before.TotalAlloc, err)
	}
	concrete := display.(*docToolDisplay)
	if concrete.spanCount != 1 {
		test.Fatal("plain PRE bytes should use a single provenance run", concrete.spanCount)
	}
	runtime.KeepAlive(display)
	escaped := docToolDisplayTestRaw(test, CIREvent{Kind: EventToolResult, Output: strings.Repeat("<\n🙂", 4000)})
	display, err = BuildDocToolDisplay(context.Background(), escaped)
	if err != nil {
		test.Fatal(err)
	}
	allocations = testing.AllocsPerRun(8, func() {
		_, failure = display.SourceRange(len(display.Text())-4, 4)
	})
	if failure != nil || allocations != 0 {
		test.Fatal("provenance lookup reparses or allocates", allocations, failure)
	}
}

func BenchmarkDocToolDisplayPlain(benchmark *testing.B) {
	raw := docToolDisplayTestRaw(benchmark, CIREvent{Kind: EventToolResult, Output: strings.Repeat("x", 120<<10)})
	benchmark.ReportAllocs()
	benchmark.SetBytes(int64(len(raw)))
	benchmark.ResetTimer()
	for benchmark.Loop() {
		if _, err := BuildDocToolDisplay(context.Background(), raw); err != nil {
			benchmark.Fatal(err)
		}
	}
}

func BenchmarkDocToolDisplayProvenance(benchmark *testing.B) {
	raw := docToolDisplayTestRaw(benchmark, CIREvent{Kind: EventToolResult, Output: strings.Repeat("<\n🙂", 4000)})
	display, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil {
		benchmark.Fatal(err)
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for benchmark.Loop() {
		if _, err := display.SourceRange(len(display.Text())-4, 4); err != nil {
			benchmark.Fatal(err)
		}
	}
}
