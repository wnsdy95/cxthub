package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

type docFindSourceTestField struct {
	text  string
	token string
}

func TestLocateDocFindTextSourceFieldOrder(testContext *testing.T) {
	cases := []struct {
		name   string
		raw    string
		fields []docFindSourceTestField
	}{
		{
			name: "message escapes and skipped blocks",
			raw:  " \n" + "{\"kind\":\"message\",\"locked\":{\"blob\":\"never search\"},\"blocks\":[null,{\"type\":\"image\",\"text\":\"hidden\"},{\"type\":\"text\",\"text\":\"\"},{\"type\":\"text\",\"text\":\"\ud55c🙂\\n\\u003c\\u003e\\u0026\\\"\\\\\\/\\b\\f\\r\\t\\u2028\\u2029\"},{\"type\":\"text\",\"text\":\"tail\"}]}" + "\t",
			fields: []docFindSourceTestField{
				{"\ud55c🙂\n<>&\"\\/\b\f\r\t\u2028\u2029", "\"\ud55c🙂\\n\\u003c\\u003e\\u0026\\\"\\\\\\/\\b\\f\\r\\t\\u2028\\u2029\""},
				{"tail", `"tail"`},
			},
		},
		{
			name:   "turn follows message order",
			raw:    `{"kind":"turn","blocks":[{"type":"text","text":"turn text"}]}`,
			fields: []docFindSourceTestField{{"turn text", `"turn text"`}},
		},
		{
			name:   "reasoning ignores opaque provider state",
			raw:    "{\"kind\":\"reasoning\",\"locked\":{\"blob\":\"opaque\",\"nested\":[1e9999]},\"replacement\":[{\"kind\":\"message\",\"blocks\":[{\"type\":\"text\",\"text\":\"hidden\"}]}],\"redacted_summary\":\"visible\\n\ud55c\"}",
			fields: []docFindSourceTestField{{"visible\n\ud55c", "\"visible\\n\ud55c\""}},
		},
		{
			name: "provider name then sorted recursive input",
			raw:  "{\"kind\":\"tool_call\",\"tool_name\":\"shell\",\"provider_tool_name\":\"Bash\",\"input\":{\"z\":[true,null,\"\",{\"v\":\"finish\"}],\"n\":1.00,\"a\":[\"x\",{\"β\":\"\ud55c\"}],\"\":\"empty-key-value\",\"empty\":\"\"}}",
			fields: []docFindSourceTestField{
				{"Bash", `"Bash"`}, {"empty-key-value", `"empty-key-value"`},
				{"a", `"a"`}, {"x", `"x"`}, {"β", `"β"`}, {"\ud55c", "\"\ud55c\""},
				{"empty", `"empty"`}, {"n", `"n"`}, {"1", `1.00`},
				{"z", `"z"`}, {"true", `true`}, {"null", `null`}, {"v", `"v"`}, {"finish", `"finish"`},
			},
		},
		{
			name:   "fallback tool name",
			raw:    `{"kind":"tool_call","provider_tool_name":"","tool_name":"shell","input":null}`,
			fields: []docFindSourceTestField{{"shell", `"shell"`}},
		},
		{
			name:   "null provider name falls back",
			raw:    `{"kind":"tool_call","provider_tool_name":null,"tool_name":"shell"}`,
			fields: []docFindSourceTestField{{"shell", `"shell"`}},
		},
		{
			name: "output primitives normalize without source identity rewriting",
			raw:  `{"kind":"tool_result","output":{"source_id":"unchanged","x":[1e20,-0.0,9007199254740993,1E+02,1e-7,false,null,"<&"],"":"empty"}}`,
			fields: []docFindSourceTestField{
				{"empty", `"empty"`}, {"source_id", `"source_id"`}, {"unchanged", `"unchanged"`}, {"x", `"x"`},
				{"100000000000000000000", `1e20`}, {"-0", `-0.0`}, {"9007199254740992", `9007199254740993`},
				{"100", `1E+02`}, {"1e-7", `1e-7`}, {"false", `false`}, {"null", `null`}, {"<&", `"<&"`},
			},
		},
		{
			name:   "scalar output",
			raw:    "{\"kind\":\"tool_result\",\"output\":\"result\\n\ud55c🙂\"}",
			fields: []docFindSourceTestField{{"result\n\ud55c🙂", "\"result\\n\ud55c🙂\""}},
		},
		{
			name:   "numeric output",
			raw:    `{"kind":"tool_result","output":3.0}`,
			fields: []docFindSourceTestField{{"3", `3.0`}},
		},
		{
			name:   "boolean output",
			raw:    `{"kind":"tool_result","output":true}`,
			fields: []docFindSourceTestField{{"true", `true`}},
		},
	}
	for _, scenario := range cases {
		testContext.Run(scenario.name, func(testContext *testing.T) {
			raw := []byte(scenario.raw)
			for field, expected := range scenario.fields {
				from := strings.LastIndex(scenario.raw, expected.token)
				if from < 0 {
					testContext.Fatal("missing expected physical token", expected.token)
				}
				span := DocFindSpan{From: from, To: from + len(expected.token)}
				if expected.token[0] == '"' {
					span.From++
					span.To--
				}
				got, err := LocateDocFindTextSource(raw, field, 0, len(expected.text))
				if err != nil || got != span {
					testContext.Fatalf("field %d (%q): got %+v, %v; want %+v", field, expected.text, got, err, span)
				}
				for offset, character := range expected.text {
					length := utf8.RuneLen(character)
					got, err := LocateDocFindTextSource(raw, field, offset, length)
					if err != nil {
						testContext.Fatalf("field %d byte %d: %v", field, offset, err)
					}
					if expected.token[0] == '"' {
						docFindSourceAssertDecoded(testContext, raw, got, expected.text[offset:offset+length])
					} else if expected.token != expected.text {
						if got != span {
							testContext.Fatalf("normalized primitive split: got %+v, want %+v", got, span)
						}
					} else if got != (DocFindSpan{From: span.From + offset, To: span.From + offset + length}) {
						testContext.Fatal("literal bytes do not match cursor", got, span, offset, length)
					}
				}
			}
			if _, err := LocateDocFindTextSource(raw, len(scenario.fields), 0, 1); !errors.Is(err, ErrValidation) {
				testContext.Fatal("outside field accepted", err)
			}
		})
	}
}

func TestLocateDocFindTextSourceLastKeyWins(testContext *testing.T) {
	cases := []struct {
		raw    string
		fields []docFindSourceTestField
	}{
		{
			raw:    `{"kind":"message","kind":"reasoning","redacted_summary":"old","redacted\u005fsummary":"winner","nested":{"redacted_summary":"decoy"}}`,
			fields: []docFindSourceTestField{{"winner", `"winner"`}},
		},
		{
			raw:    `{"kind":"message","blocks":[{"type":"text","text":"discarded"}],"bl\u006fcks":[{"type":"other","ty\u0070e":"text","text":"old","te\u0078t":"winner"}]}`,
			fields: []docFindSourceTestField{{"winner", `"winner"`}},
		},
		{
			raw:    `{"kind":"tool_call","tool_name":"old","tool\u005fname":"tool","input":{"a":{"discarded":"hidden"},"\u0061":{"b":"kept"},"z":false}}`,
			fields: []docFindSourceTestField{{"tool", `"tool"`}, {"a", `"\u0061"`}, {"b", `"b"`}, {"kept", `"kept"`}, {"z", `"z"`}, {"false", `false`}},
		},
		{
			raw:    `{"kind":"tool_result","output":{"\ud800":"discarded","\ufffd":"kept"}}`,
			fields: []docFindSourceTestField{{"�", `"\ufffd"`}, {"kept", `"kept"`}},
		},
		{
			raw:    `{"kind":"tool_result","output":{"z":"discarded","z":"kept","a":null}}`,
			fields: []docFindSourceTestField{{"a", `"a"`}, {"null", `null`}, {"z", `"z"`}, {"kept", `"kept"`}},
		},
		{
			raw:    `{"kind":"tool_call","input":{"hidden":"discarded"},"in\u0070ut":{"kept":"value"}}`,
			fields: []docFindSourceTestField{{"kept", `"kept"`}, {"value", `"value"`}},
		},
		{
			raw:    `{"kind":"tool_result","output":"discarded","out\u0070ut":"kept"}`,
			fields: []docFindSourceTestField{{"kept", `"kept"`}},
		},
	}
	for _, scenario := range cases {
		for field, expected := range scenario.fields {
			from := strings.LastIndex(scenario.raw, expected.token)
			want := DocFindSpan{From: from, To: from + len(expected.token)}
			if expected.token[0] == '"' {
				want.From++
				want.To--
			}
			got, err := LocateDocFindTextSource([]byte(scenario.raw), field, 0, len(expected.text))
			if err != nil || got != want {
				testContext.Fatalf("%s field %d: got %+v, %v; want %+v", scenario.raw, field, got, err, want)
			}
		}
		if _, err := LocateDocFindTextSource([]byte(scenario.raw), len(scenario.fields), 0, 1); !errors.Is(err, ErrValidation) {
			testContext.Fatal("overwritten fields were enumerated", scenario.raw, err)
		}
	}
}

func TestLocateDocFindSourceSurrogateParity(testContext *testing.T) {
	for _, encoded := range []string{
		`"\ud800"`, `"\udc00"`, `"\ud800x\udc00"`, `"\uD83D\uDE42"`,
		`"\uD83D\uD83D\uDE00"`, `"\udc00\ud800"`, `"\ud800\u0041\udc00"`,
		"\"\ud55c\\ud800🙂\\ufffd\\udc00\ub05d\"", `"\u0000\n\u003C\u003e\u0026"`,
	} {
		var decoded string
		if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
			testContext.Fatal(err)
		}
		for begin := range decoded {
			for end := begin + 1; end <= len(decoded); end++ {
				if end < len(decoded) && !utf8.RuneStart(decoded[end]) {
					continue
				}
				span, err := LocateDocFindPublicTextSource([]byte(" \n"+encoded+"\t"), EventReasoning, 0, begin, end-begin)
				if err != nil {
					testContext.Fatalf("%s byte %d:%d: %v", encoded, begin, end, err)
				}
				docFindSourceAssertDecoded(testContext, []byte(" \n"+encoded+"\t"), span, decoded[begin:end])
			}
		}
	}
}

func TestLocateDocFindPublicTextSourceMatchesWholeEvent(testContext *testing.T) {
	for _, scenario := range []struct {
		kind EventKind
		raw  string
	}{
		{EventReasoning, "\"visible\\n\\u003c\ud55c\\uD83D\\uDE42\\ud800tail\""},
		{EventMessage, "[{\"type\":\"image\",\"text\":\"hidden\"},{\"type\":\"text\",\"text\":\"\"},{\"type\":\"text\",\"text\":\"\uccab\\n\\u003c🙂\"},{\"type\":\"text\",\"text\":\"second\"}]"},
		{EventTurn, `[{"type":"text","text":"turn"}]`},
	} {
		name := "blocks"
		var fields []string
		if scenario.kind == EventReasoning {
			name = "redacted_summary"
			var text string
			if err := json.Unmarshal([]byte(scenario.raw), &text); err != nil {
				testContext.Fatal(err)
			}
			fields = []string{text}
		} else {
			var blocks []ContentBlock
			if err := json.Unmarshal([]byte(scenario.raw), &blocks); err != nil {
				testContext.Fatal(err)
			}
			fields = DocFindTexts(CIREvent{Kind: scenario.kind, Blocks: blocks})
		}
		prefix := fmt.Sprintf(" \n{\"kind\":%q,\"%s\":", scenario.kind, name)
		whole := []byte(prefix + scenario.raw + `,"locked":{"blob":"unrelated"}}`)
		public := []byte(" \t" + scenario.raw + "\n")
		for field, text := range fields {
			for offset, character := range text {
				length := utf8.RuneLen(character)
				span, err := LocateDocFindPublicTextSource(public, scenario.kind, field, offset, length)
				if err != nil {
					testContext.Fatal(err)
				}
				wholeSpan, err := LocateDocFindTextSource(whole, field, offset, length)
				want := DocFindSpan{From: span.From - 2 + len(prefix), To: span.To - 2 + len(prefix)}
				if err != nil || wholeSpan != want {
					testContext.Fatal("public and whole event disagree", span, wholeSpan, want, err)
				}
				docFindSourceAssertDecoded(testContext, public, span, text[offset:offset+length])
			}
		}
	}
}

func TestLocateDocFindSourceRejectsMalformedAndOutsideCursors(testContext *testing.T) {
	for _, raw := range [][]byte{
		nil, []byte(" \n"), []byte(`{"kind":"reasoning","redacted_summary":"unterminated}`),
		[]byte(`{"kind":"reasoning","redacted_summary":"\x20"}`),
		[]byte(`{"kind":"reasoning","redacted_summary":"\u00Q0"}`),
		[]byte(`{"kind":"reasoning","redacted_summary":"ok"} {}`),
		[]byte(`{"kind":"message","blocks":[{"type":"text","text":"ok"},]}`),
		append([]byte(`{"kind":"reasoning","redacted_summary":"`), 0xff, '"', '}'),
		[]byte(`{"kind":"reasoning","redacted_summary":"raw` + "\n" + `newline"}`),
		[]byte(strings.Repeat(" ", maxDocFindSourceBytes) + `{}`),
		[]byte(`null`), []byte(`[]`), []byte(`"event"`),
		[]byte(`{"kind":"reasoning","redacted_summary":123}`),
		[]byte(`{"kind":"message","blocks":{}}`),
		[]byte(`{"kind":"message","blocks":[true]}`),
		[]byte(`{"kind":"message","blocks":[{"type":true,"text":"ok"}]}`),
		[]byte(`{"kind":"message","blocks":[{"type":"text","text":"ok"},{"type":"other","text":1}]}`),
		[]byte(`{"kind":"tool_call","provider_tool_name":true,"tool_name":"ok"}`),
		[]byte(`{"kind":"tool_call","provider_tool_name":"ok","tool_name":false}`),
		[]byte(`{"kind":"tool_call","tool_name":"ok","input":[]}`),
		[]byte(`{"kind":"tool_result","output":["ok",1e9999]}`),
	} {
		if span, err := LocateDocFindTextSource(raw, 0, 0, 1); err == nil || span != (DocFindSpan{}) {
			testContext.Fatalf("malformed source accepted: %q: %+v %v", raw, span, err)
		}
	}
	for _, raw := range []string{
		`{"kind":"compaction","redacted_summary":"hidden","replacement":[{"kind":"message","blocks":[{"type":"text","text":"hidden"}]}]}`,
		`{"kind":"reasoning","locked":{"blob":"hidden"}}`,
		`{"kind":"reasoning","redacted_summary":""}`,
		`{"kind":"message","blocks":[{"type":"other","text":"hidden"},{"type":"text","text":""}]}`,
		`{"kind":"tool_call","provider_tool_name":"","tool_name":"","input":{}}`,
		`{"kind":"tool_result","output":null}`,
		`{"kind":"tool_result","output":["",{},[]]}`,
		`{"kind":"reasoning","redacted_summary":"old","redacted_summary":null}`,
	} {
		if _, err := LocateDocFindTextSource([]byte(raw), 0, 0, 1); !errors.Is(err, ErrValidation) {
			testContext.Fatal("absent field accepted", raw, err)
		}
	}
	raw := []byte("{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c🙂x\"}")
	for _, cursor := range [][3]int{
		{-1, 0, 1}, {1, 0, 1}, {0, -1, 1}, {0, 0, 0}, {0, 0, -1}, {0, 1, 2},
		{0, 0, 1}, {0, 3, 1}, {0, 4, 3}, {0, 7, 2}, {0, 8, 1},
		{0, int(^uint(0) >> 1), 1}, {0, 1, int(^uint(0) >> 1)},
	} {
		if span, err := LocateDocFindTextSource(raw, cursor[0], cursor[1], cursor[2]); !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			testContext.Fatal("outside or split rune accepted", cursor, span, err)
		}
	}
	for _, scenario := range []struct {
		raw  string
		kind EventKind
	}{
		{`null`, EventReasoning}, {`123`, EventReasoning}, {`[]`, EventReasoning},
		{`{}`, EventMessage}, {`null`, EventMessage}, {`"text"`, EventMessage},
		{`"text"`, EventToolCall}, {`"text"`, EventCompaction},
		{`[{"type":"text","text":"ok"},true]`, EventMessage},
	} {
		if _, err := LocateDocFindPublicTextSource([]byte(scenario.raw), scenario.kind, 0, 0, 1); err == nil {
			testContext.Fatal("malformed public source accepted", scenario, err)
		}
	}
}

func TestLocateDocFindSourceBoundAndOpaqueAllocations(testContext *testing.T) {
	const prefix = `{"kind":"reasoning","redacted_summary":"visible","locked":{"blob":"`
	const suffix = `"}}`
	small := []byte(prefix + "opaque" + suffix)
	large := []byte(prefix + strings.Repeat("x", maxDocFindSourceBytes-len(prefix)-len(suffix)) + suffix)
	before := bytes.Clone(large)
	for _, raw := range [][]byte{small, large} {
		span, err := LocateDocFindTextSource(raw, 0, 0, len("visible"))
		if err != nil || string(raw[span.From:span.To]) != "visible" {
			testContext.Fatal("bounded opaque source rejected", span, err)
		}
	}
	if !bytes.Equal(large, before) {
		testContext.Fatal("source was modified")
	}
	measure := func(raw []byte) float64 {
		return testing.AllocsPerRun(30, func() {
			if _, err := LocateDocFindTextSource(raw, 0, 0, len("visible")); err != nil {
				panic(err)
			}
		})
	}
	smallAllocs, largeAllocs := measure(small), measure(large)
	if largeAllocs > smallAllocs+1 || largeAllocs > 4 {
		testContext.Fatal("opaque state increased allocations", smallAllocs, largeAllocs)
	}
	var beforeMemory, afterMemory runtime.MemStats
	runtime.ReadMemStats(&beforeMemory)
	for iteration := 0; iteration < 10; iteration++ {
		if _, err := LocateDocFindTextSource(large, 0, 0, len("visible")); err != nil {
			testContext.Fatal(err)
		}
	}
	runtime.ReadMemStats(&afterMemory)
	if allocated := (afterMemory.TotalAlloc - beforeMemory.TotalAlloc) / 10; allocated > 16<<10 {
		testContext.Fatal("opaque state was copied", allocated)
	}
	if _, err := LocateDocFindTextSource(append(large, ' '), 0, 0, 1); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("source above 128 KiB accepted", err)
	}
	public := []byte(`"` + strings.Repeat("a", maxDocFindSourceBytes-2) + `"`)
	if _, err := LocateDocFindPublicTextSource(public, EventReasoning, 0, maxDocFindSourceBytes-3, 1); err != nil {
		testContext.Fatal("public source at bound rejected", err)
	}
	if _, err := LocateDocFindPublicTextSource(append(public, ' '), EventReasoning, 0, 0, 1); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("public source above bound accepted", err)
	}
}

func TestLocateDocFindTextSourceMatchesDocFindTexts(testContext *testing.T) {
	random := rand.New(rand.NewSource(95))
	var generate func(int) any
	generate = func(depth int) any {
		choice := random.Intn(6)
		if depth == 0 {
			choice = random.Intn(4)
		}
		switch choice {
		case 0:
			return []string{"", "\ud55c🙂\n<>&", "alpha", "�", "\"\\\t"}[random.Intn(5)]
		case 1:
			return nil
		case 2:
			return random.Intn(2) == 1
		case 3:
			return float64(random.Intn(1000)) / 7
		case 4:
			return []any{generate(depth - 1), generate(depth - 1), generate(depth - 1)}
		default:
			return map[string]any{"": generate(depth - 1), "β": generate(depth - 1), "a\n<": generate(depth - 1)}
		}
	}
	for iteration := 0; iteration < 80; iteration++ {
		event := CIREvent{Kind: EventToolResult, Output: generate(3)}
		if iteration%2 == 0 {
			event = CIREvent{Kind: EventToolCall, ToolName: "shell", Input: map[string]any{"z": generate(3), "a": generate(3)}}
		}
		raw, err := json.Marshal(event)
		if err != nil {
			testContext.Fatal(err)
		}
		var decoded CIREvent
		if err := json.Unmarshal(raw, &decoded); err != nil {
			testContext.Fatal(err)
		}
		fields := DocFindTexts(decoded)
		for field, text := range fields {
			span, err := LocateDocFindTextSource(raw, field, 0, len(text))
			if err != nil {
				testContext.Fatal("field missing", iteration, field, text, err)
			}
			physical := raw[span.From:span.To]
			var decodedText string
			quoted := append(append([]byte{'"'}, physical...), '"')
			if json.Unmarshal(quoted, &decodedText) == nil && decodedText == text {
				continue
			}
			var primitive any
			if err := json.Unmarshal(physical, &primitive); err != nil {
				testContext.Fatal("neither string nor primitive source", text, string(physical), err)
			}
			normalized, err := json.Marshal(primitive)
			if err != nil || string(normalized) != text {
				testContext.Fatal("DocFindTexts order or normalization differs", field, text, string(normalized), err)
			}
		}
		if _, err := LocateDocFindTextSource(raw, len(fields), 0, 1); !errors.Is(err, ErrValidation) {
			testContext.Fatal("extra field found", iteration, fields, err)
		}
	}
}

func TestLocateDocFindTextSourceDeepAndWide(testContext *testing.T) {
	deep := []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 9998) + `"leaf"` + strings.Repeat("]", 9998) + `}`)
	span, err := LocateDocFindTextSource(deep, 0, 0, 4)
	if err != nil || string(deep[span.From:span.To]) != "leaf" {
		testContext.Fatal("valid deeply nested source rejected", span, err)
	}
	tooDeep := []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 10000) + `"leaf"` + strings.Repeat("]", 10000) + `}`)
	if _, err := LocateDocFindTextSource(tooDeep, 0, 0, 4); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("encoding/json depth limit bypassed", err)
	}
	var source strings.Builder
	source.WriteString(`{"kind":"tool_result","output":{`)
	for position := 4095; position >= 0; position-- {
		if position != 4095 {
			source.WriteByte(',')
		}
		fmt.Fprintf(&source, `"key%04d":"value%04d"`, position, position)
	}
	source.WriteString(`}}`)
	raw := []byte(source.String())
	for _, field := range []int{0, 1, 4096, 4097, 8190, 8191} {
		text := fmt.Sprintf("key%04d", field/2)
		if field%2 == 1 {
			text = fmt.Sprintf("value%04d", field/2)
		}
		span, err := LocateDocFindTextSource(raw, field, 0, len(text))
		if err != nil || string(raw[span.From:span.To]) != text {
			testContext.Fatal("wide sorted object mismatch", field, span, text, err)
		}
	}
}

func docFindSourceAssertDecoded(testContext *testing.T, raw []byte, span DocFindSpan, want string) {
	testContext.Helper()
	if span.From < 0 || span.To <= span.From || span.To > len(raw) {
		testContext.Fatal("invalid physical span", span, len(raw))
	}
	quoted := append(append([]byte{'"'}, raw[span.From:span.To]...), '"')
	var got string
	if err := json.Unmarshal(quoted, &got); err != nil || got != want {
		testContext.Fatalf("physical %q decoded %q, %v; want %q", raw[span.From:span.To], got, err, want)
	}
}

func TestLocateDocFindTextSourcesBatchParityAndOrder(testContext *testing.T) {
	for _, raw := range []string{
		"{\"kind\":\"tool_result\",\"output\":{\"z\":[true,null,1.00,1e20,9007199254740993],\"a\":\"\ud55c\\n\\u003c\\uD83D\\uDE42\\ud800\",\"b\":{\"d\":\"second\",\"c\":\"third\"}}}",
		"{\"kind\":\"message\",\"blocks\":[{\"type\":\"text\",\"text\":\"\ud55c\\n<🙂\"},{\"type\":\"image\",\"text\":\"skip\"},{\"type\":\"text\",\"text\":\"second field\"}]}",
		"{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c\\n\\u003c\\uD83D\\uDE42\\ud800tail\"}",
		`{"kind":"tool_call","provider_tool_name":"tool","tool_name":"ignored","input":{"z":[null,true],"a":"first"}}`,
	} {
		var event CIREvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			testContext.Fatal(err)
		}
		fields := DocFindTexts(event)
		var matches []DocFindMatch
		for field, text := range fields {
			if len(matches) == DocFindPageMatches {
				break
			}
			matches = append(matches, DocFindMatch{Field: field, Length: len(text), Preview: "unchanged", Index: 17})
			for offset, character := range text {
				if len(matches) == DocFindPageMatches {
					break
				}
				matches = append(matches, DocFindMatch{Field: field, Offset: offset, Length: utf8.RuneLen(character), Preview: "unchanged"})
			}
		}
		for left, right := 0, len(matches)-1; left < right; left, right = left+1, right-1 {
			matches[left], matches[right] = matches[right], matches[left]
		}
		before := append([]DocFindMatch(nil), matches...)
		spans, err := LocateDocFindTextSources([]byte(raw), matches)
		if err != nil || len(spans) != len(matches) {
			testContext.Fatal("batch failed", len(spans), err)
		}
		for position, match := range matches {
			want, err := LocateDocFindTextSource([]byte(raw), match.Field, match.Offset, match.Length)
			if err != nil || spans[position] != want {
				testContext.Fatal("batch order or mapping differs", position, spans[position], want, err)
			}
		}
		if !reflect.DeepEqual(matches, before) {
			testContext.Fatal("logical match metadata was changed")
		}
	}
	raw := []byte(`{"kind":"tool_result","output":{"a":"discarded","\u0061":"kept","z":[null,true]}}`)
	matches := []DocFindMatch{{Field: 4, Length: 4}, {Field: 0, Length: 1}, {Field: 1, Length: 4}, {Field: 3, Length: 4}}
	spans, err := LocateDocFindTextSources(raw, matches)
	if err != nil {
		testContext.Fatal(err)
	}
	for position, want := range []string{"true", `\u0061`, "kept", "null"} {
		if got := string(raw[spans[position].From:spans[position].To]); got != want {
			testContext.Fatal("batch did not use last keys or sorted fields", position, got, want)
		}
	}
}

func TestLocateDocFindPublicTextSourcesBatch(testContext *testing.T) {
	for _, scenario := range []struct {
		raw  string
		kind EventKind
	}{
		{" \"\ud55c\\n\\u003c\\uD83D\\uDE42\\ud800tail\" ", EventReasoning},
		{" [{\"type\":\"image\",\"text\":\"hidden\"},{\"type\":\"text\",\"text\":\"\ud55c\\n\\u003c🙂\"},{\"type\":\"text\",\"text\":\"second\"}] ", EventMessage},
	} {
		var fields []string
		if scenario.kind == EventReasoning {
			var text string
			if err := json.Unmarshal([]byte(scenario.raw), &text); err != nil {
				testContext.Fatal(err)
			}
			fields = []string{text}
		} else {
			var blocks []ContentBlock
			if err := json.Unmarshal([]byte(scenario.raw), &blocks); err != nil {
				testContext.Fatal(err)
			}
			fields = DocFindTexts(CIREvent{Kind: scenario.kind, Blocks: blocks})
		}
		matches := make([]DocFindMatch, DocFindPageMatches)
		for position := range matches {
			field := position % len(fields)
			matches[position] = DocFindMatch{Field: field, Length: len(fields[field])}
			if position%2 == 0 {
				_, length := utf8.DecodeLastRuneInString(fields[field])
				matches[position].Offset = len(fields[field]) - length
				matches[position].Length = length
			}
		}
		spans, err := LocateDocFindPublicTextSources([]byte(scenario.raw), scenario.kind, matches)
		if err != nil || len(spans) != len(matches) {
			testContext.Fatal("public batch failed", err)
		}
		for position, match := range matches {
			text := fields[match.Field][match.Offset : match.Offset+match.Length]
			docFindSourceAssertDecoded(testContext, []byte(scenario.raw), spans[position], text)
		}
	}
}

func TestLocateDocFindTextSourcesBatchRejectsInvalid(testContext *testing.T) {
	raw := []byte("{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c🙂text\"}")
	for _, matches := range [][]DocFindMatch{
		make([]DocFindMatch, DocFindPageMatches+1),
		{{Field: 0, Length: 3}, {Field: 1, Length: 1}},
		{{Field: 0, Length: 3}, {Field: 0, Offset: 1, Length: 2}},
		{{Field: 0, Length: 3}, {Field: 0, Offset: 3, Length: 1}},
		{{Field: 0, Length: 3}, {Field: -1, Length: 1}},
		{{Field: 0, Length: 3}, {Field: 0, Offset: -1, Length: 1}},
		{{Field: 0, Length: 3}, {Field: 0, Length: 0}},
		{{Field: 0, Length: 3}, {Field: 0, Offset: 9999, Length: 1}},
		{{Field: 0, Length: 3}, {Field: 0, Offset: 1, Length: int(^uint(0) >> 1)}},
	} {
		if spans, err := LocateDocFindTextSources(raw, matches); spans != nil || !errors.Is(err, ErrValidation) {
			testContext.Fatal("invalid batch returned partial results", spans, err)
		}
		if spans, err := LocateDocFindPublicTextSources([]byte("\"\ud55c🙂text\""), EventReasoning, matches); spans != nil || !errors.Is(err, ErrValidation) {
			testContext.Fatal("invalid public batch returned partial results", spans, err)
		}
	}
	if spans, err := LocateDocFindTextSources(raw, nil); err != nil || len(spans) != 0 {
		testContext.Fatal("empty batch failed", spans, err)
	}
	if spans, err := LocateDocFindPublicTextSources([]byte(`""`), EventReasoning, nil); err != nil || len(spans) != 0 {
		testContext.Fatal("empty public batch failed", spans, err)
	}
	if spans, err := LocateDocFindTextSources([]byte(`{"broken":`), []DocFindMatch{{Length: 1}}); spans != nil || !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("malformed batch source accepted", spans, err)
	}
}

func docFindSourceBatchFixtures() (dense []byte, nested []byte, denseMatches []DocFindMatch, nestedMatches []DocFindMatch) {
	const prefix = `{"kind":"tool_result","output":[`
	const suffix = `]}`
	count := (maxDocFindSourceBytes - len(prefix) - len(suffix) + 1) / 2
	dense = []byte(prefix + strings.Repeat("0,", count-1) + "0" + suffix)
	items := make([]any, 256)
	for position := range items {
		fields := make(map[string]any, 32)
		for field := 0; field < 32; field++ {
			fields[fmt.Sprintf("k%03d", field)] = fmt.Sprintf("v%03d", field)
		}
		items[position] = fields
	}
	nested, _ = json.Marshal(CIREvent{Kind: EventToolResult, Output: map[string]any{"nested": items}})
	for position := 0; position < DocFindPageMatches; position++ {
		denseMatches = append(denseMatches, DocFindMatch{Field: count - 1 - position, Length: 1})
		nestedMatches = append(nestedMatches, DocFindMatch{Field: 1 + (255-position)*64 + 63, Length: 4})
	}
	return dense, nested, denseMatches, nestedMatches
}

func docFindSourceWideFixture() ([]byte, []DocFindMatch) {
	var source strings.Builder
	source.WriteString(`{"kind":"tool_result","output":{`)
	count := 0
	for source.Len()+len(`"ffff":0`)+len(`}}`) < maxDocFindSourceBytes {
		if count > 0 {
			source.WriteByte(',')
		}
		fmt.Fprintf(&source, `"%04x":0`, count)
		count++
	}
	source.WriteString(`}}`)
	matches := make([]DocFindMatch, DocFindPageMatches)
	for position := range matches {
		matches[position] = DocFindMatch{Field: (count-1-position)*2 + 1, Length: 1}
	}
	return []byte(source.String()), matches
}

func TestLocateDocFindTextSourcesBatchAllocationBound(testContext *testing.T) {
	dense, nested, denseMatches, nestedMatches := docFindSourceBatchFixtures()
	wide, wideMatches := docFindSourceWideFixture()
	for _, scenario := range []struct {
		name    string
		raw     []byte
		matches []DocFindMatch
		limit   uint64
	}{
		{"dense 128KiB twenty matches", dense, denseMatches, 768 << 10},
		{"nested wide twenty matches", nested, nestedMatches, 3 << 20},
		{"flat wide 128KiB twenty matches", wide, wideMatches, 4 << 20},
	} {
		testContext.Run(scenario.name, func(testContext *testing.T) {
			if len(scenario.raw) > maxDocFindSourceBytes {
				testContext.Fatal("fixture exceeds bound", len(scenario.raw))
			}
			for iteration := 0; iteration < 2; iteration++ {
				if _, err := LocateDocFindTextSources(scenario.raw, scenario.matches); err != nil {
					testContext.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for iteration := 0; iteration < 5; iteration++ {
				spans, err := LocateDocFindTextSources(scenario.raw, scenario.matches)
				if err != nil || len(spans) != DocFindPageMatches {
					testContext.Fatal("bounded batch failed", len(spans), err)
				}
				want := "0"
				if scenario.name == "nested wide twenty matches" {
					want = "v031"
				}
				for _, span := range spans {
					if string(scenario.raw[span.From:span.To]) != want {
						testContext.Fatal("bounded batch field mismatch", span, want)
					}
				}
			}
			runtime.ReadMemStats(&after)
			allocated := (after.TotalAlloc - before.TotalAlloc) / 5
			if allocated > scenario.limit {
				testContext.Fatal("batch allocation regression", allocated, scenario.limit)
			}
			testContext.Logf("%d source bytes, twenty matches: %d allocated bytes/batch", len(scenario.raw), allocated)
		})
	}
}

func BenchmarkLocateDocFindTextSources(benchmark *testing.B) {
	dense, nested, denseMatches, nestedMatches := docFindSourceBatchFixtures()
	wide, wideMatches := docFindSourceWideFixture()
	firstMatches := make([]DocFindMatch, DocFindPageMatches)
	for position := range firstMatches {
		firstMatches[position] = DocFindMatch{Field: position, Length: 1}
	}
	for _, scenario := range []struct {
		name    string
		raw     []byte
		matches []DocFindMatch
	}{
		{"dense_128KiB_first20", dense, firstMatches},
		{"dense_128KiB_last20", dense, denseMatches},
		{"nested_wide_20", nested, nestedMatches},
		{"wide_128KiB_20", wide, wideMatches},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(scenario.raw)))
			for benchmark.Loop() {
				if _, err := LocateDocFindTextSources(scenario.raw, scenario.matches); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLocateDocFindPublicTextSources(benchmark *testing.B) {
	raw := []byte(`"` + strings.Repeat(`\u003c`, (maxDocFindSourceBytes-2)/6) + `"`)
	matches := make([]DocFindMatch, DocFindPageMatches)
	for position := range matches {
		matches[position] = DocFindMatch{Offset: (len(raw)-2)/6 - 1 - position, Length: 1}
	}
	benchmark.ReportAllocs()
	benchmark.SetBytes(int64(len(raw)))
	for benchmark.Loop() {
		if _, err := LocateDocFindPublicTextSources(raw, EventReasoning, matches); err != nil {
			benchmark.Fatal(err)
		}
	}
}

func BenchmarkLocateDocFindTextSource(benchmark *testing.B) {
	wide := make(map[string]any, 2048)
	for position := 0; position < 2048; position++ {
		wide[fmt.Sprintf("key%04d", position)] = fmt.Sprintf("value%04d", position)
	}
	wideRaw, err := json.Marshal(CIREvent{Kind: EventToolResult, Output: wide})
	if err != nil {
		benchmark.Fatal(err)
	}
	const opaquePrefix = `{"kind":"reasoning","redacted_summary":"visible","locked":{"blob":"`
	const opaqueSuffix = `"}}`
	const densePrefix = `{"kind":"tool_result","output":[`
	const denseSuffix = `]}`
	denseCount := (maxDocFindSourceBytes - len(densePrefix) - len(denseSuffix) + 1) / 2
	denseRaw := []byte(densePrefix + strings.Repeat("0,", denseCount-1) + "0" + denseSuffix)
	var wideLimit strings.Builder
	wideLimit.WriteString(`{"kind":"tool_result","output":{`)
	wideCount := 0
	for wideLimit.Len()+len(`"ffff":0`)+len(`}}`) < maxDocFindSourceBytes {
		if wideCount > 0 {
			wideLimit.WriteByte(',')
		}
		fmt.Fprintf(&wideLimit, `"%04x":0`, wideCount)
		wideCount++
	}
	wideLimit.WriteString(`}}`)
	for _, scenario := range []struct {
		name  string
		raw   []byte
		field int
	}{
		{"summary_small", []byte(`{"kind":"reasoning","redacted_summary":"visible"}`), 0},
		{"summary_opaque_128KiB", []byte(opaquePrefix + strings.Repeat("x", maxDocFindSourceBytes-len(opaquePrefix)-len(opaqueSuffix)) + opaqueSuffix), 0},
		{"wide_2048", wideRaw, 4095},
		{"deep_2048", []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 2048) + `"leaf"` + strings.Repeat("]", 2048) + `}`), 0},
		{"dense_near_128KiB", denseRaw, denseCount - 1},
		{"wide_near_128KiB", []byte(wideLimit.String()), wideCount*2 - 1},
		{"deep_9998", []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 9998) + `"leaf"` + strings.Repeat("]", 9998) + `}`), 0},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(scenario.raw)))
			for benchmark.Loop() {
				if _, err := LocateDocFindTextSource(scenario.raw, scenario.field, 0, 1); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}

func FuzzLocateDocFindSource(fuzzContext *testing.F) {
	fuzzContext.Add([]byte("{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c\\n\\uD83D\\uDE42\\ud800\"}"), 0, 0, 3)
	fuzzContext.Add([]byte(`{"kind":"tool_result","output":{"b":[true,null,1.00],"a":"x"}}`), 0, 0, 1)
	fuzzContext.Add([]byte("\"\\ud800\ud55c\""), 0, 0, 3)
	fuzzContext.Fuzz(func(testContext *testing.T, raw []byte, field, offset, length int) {
		before := bytes.Clone(raw)
		matches := []DocFindMatch{{Field: field, Offset: offset, Length: length}, {Field: 0, Length: 1}}
		span, err := LocateDocFindTextSource(raw, field, offset, length)
		if err == nil && (span.From < 0 || span.To <= span.From || span.To > len(raw)) {
			testContext.Fatal("whole source returned outside bytes", span)
		}
		for _, kind := range []EventKind{EventMessage, EventReasoning} {
			span, err := LocateDocFindPublicTextSource(raw, kind, field, offset, length)
			if err == nil && (span.From < 0 || span.To <= span.From || span.To > len(raw)) {
				testContext.Fatal("public source returned outside bytes", span)
			}
			spans, err := LocateDocFindPublicTextSources(raw, kind, matches)
			if err == nil {
				for _, span := range spans {
					if span.From < 0 || span.To <= span.From || span.To > len(raw) {
						testContext.Fatal("public batch returned outside bytes", span)
					}
				}
			}
		}
		spans, err := LocateDocFindTextSources(raw, matches)
		if err == nil {
			for _, span := range spans {
				if span.From < 0 || span.To <= span.From || span.To > len(raw) {
					testContext.Fatal("batch returned outside bytes", span)
				}
			}
		}
		if !reflect.DeepEqual(raw, before) {
			testContext.Fatal("source mutated")
		}
	})
}
