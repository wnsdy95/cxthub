package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzDocDisplayStringReplay(fuzz *testing.F) {
	for _, token := range []string{`""`, `"hello"`, "\"\ud55c\uae00🙂\"", `"\ud83d\ude42\ud800"`, `"\uFEFF \n\t"`, `"\"\\\b\f\r\t\n"`, `"` + strings.Repeat("x", DocDisplayPageTextBytes-3) + `Needle🙂"`} {
		fuzz.Add(token)
	}
	fuzz.Fuzz(func(test *testing.T, token string) {
		if len(token) > DocDisplayPageTextBytes*2 {
			return
		}
		raw := []byte(`{"kind":"tool_result","output":` + token + `}`)
		source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), strings.NewReader(string(raw)))
		var object map[string]json.RawMessage
		var kind string
		valid := utf8.Valid(raw) && json.Unmarshal(raw, &object) == nil && json.Unmarshal(object["kind"], &kind) == nil
		output := object["output"]
		if !valid || kind != "tool_result" || len(output) == 0 || output[0] != '"' {
			if err == nil {
				test.Fatal("unsupported source accepted")
			}
			return
		}
		var expected string
		if json.Unmarshal(output, &expected) != nil {
			test.Fatal("invalid oracle")
		}
		if err != nil {
			test.Fatal(err)
		}
		state, err := NewDocDisplayStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		var decoded strings.Builder
		for !state.Done {
			step, err := StepDocDisplayString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil })
			if err != nil {
				test.Fatal(err)
			}
			if step.Page != nil {
				decoded.WriteString(step.Page.Text)
				span, err := step.Page.SourceRange(0, len(step.Page.Text))
				if err != nil || span.From <= source.Token.From || span.From >= span.To || span.To >= source.Token.To {
					test.Fatal(span, err)
				}
			}
			encoded, err := step.State.Encode(source)
			if err != nil {
				test.Fatal(err)
			}
			state, err = DecodeDocDisplayStringState(source, HashContent(encoded), encoded)
			if err != nil {
				test.Fatal(err)
			}
		}
		if strings.TrimFunc(expected, docMarkdownWhitespace) == "" {
			expected = ""
		}
		if decoded.String() != expected {
			test.Fatal("checkpointed decoding differs from JSON oracle")
		}
		if _, _, err := FinishDocDisplayString(context.Background(), source, state); err != nil {
			test.Fatal(err)
		}
	})
}
