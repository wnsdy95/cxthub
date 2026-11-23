package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func displayWindowFixture(test *testing.T, text string) (DocDisplayStringSource, []DocDisplayPage, []byte) {
	test.Helper()
	raw, err := json.Marshal(map[string]any{"kind": "tool_result", "output": text})
	if err != nil {
		test.Fatal(err)
	}
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), strings.NewReader(string(raw)))
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	var pages []DocDisplayPage
	for !state.Done {
		step, err := StepDocDisplayString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil })
		if err != nil {
			test.Fatal(err)
		}
		if step.Page != nil {
			pages = append(pages, *step.Page)
		}
		state = step.State
	}
	return source, pages, raw
}
