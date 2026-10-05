package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func firstResultFixture() (map[string]json.RawMessage, *firstQuestionState) {
	raw := []byte(`{"terminal_reason":"completed","stop_reason":"end_turn","result_index":1,"user_message_uuid":"query","user_message_uuids":["query"],"subtype":"success","is_error":false,"num_turns":1,"duration_api_ms":1,"total_cost_usd":0.01,"queued_turn_count":0,"permission_denials":[],"modelUsage":{"model":{}},"usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"result":"reply"}`)
	m, _ := object(raw)
	return m, &firstQuestionState{id: "query", summary: ContextSummary{Model: "model"}}
}

func TestFirstQuestionResultRejectsForeignOrNonResponseEvidence(t *testing.T) {
	m, q := firstResultFixture()
	if answer, err := firstQuestionResult(m, q); err != nil || answer != "reply" {
		t.Fatal("valid result rejected", err)
	}
	for _, tc := range []struct{ key, raw string }{
		{"terminal_reason", `"hook_stopped"`},
		{"terminal_reason", `"tool_deferred"`},
		{"terminal_reason", `"background_requested"`},
		{"terminal_reason", `null`},
		{"stop_reason", `"max_tokens"`},
		{"stop_reason", `null`},
		{"result_index", `0`},
		{"result_index", `2`},
		{"result_index", `null`},
		{"user_message_uuid", `"other"`},
		{"user_message_uuids", `["query","other"]`},
		{"is_error", `true`},
		{"is_error", `null`},
		{"subtype", `"error_during_execution"`},
		{"num_turns", `0`},
		{"num_turns", `2`},
		{"num_turns", `1.5`},
		{"duration_api_ms", `-1`},
		{"total_cost_usd", `null`},
		{"total_cost_usd", `-1`},
		{"total_cost_usd", `1e999`},
		{"queued_turn_count", `1`},
		{"permission_denials", `[{}]`},
		{"permission_denials", `null`},
		{"usage", `{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`},
		{"usage", `{"input_tokens":null,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`},
		{"usage", `{"input_tokens":0,"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`},
		{"modelUsage", `{"other":{}}`},
		{"modelUsage", `{"model":{},"other":{}}`},
		{"modelUsage", `{"model":null}`},
		{"result", `null`},
		{"result", `""`},
	} {
		t.Run(tc.key+"/"+tc.raw, func(t *testing.T) {
			m, q := firstResultFixture()
			m[tc.key] = json.RawMessage(tc.raw)
			if answer, err := firstQuestionResult(m, q); err == nil || answer != "" {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestFirstAssistantProjectsVisibleTextOnly(t *testing.T) {
	raw := []byte(`{"role":"assistant","model":"model","content":[{"type":"thinking","thinking":"PRIVATE_REASONING"},{"type":"text","text":"visible"},{"type":"redacted_thinking","data":"PRIVATE_OPAQUE"}]}`)
	text, err := firstAssistantText(raw, "model")
	if err != nil || text != "visible" {
		t.Fatal("wrong visible text projection", err)
	}
	for _, body := range []string{
		`{"role":"assistant","model":"other","content":[{"type":"text","text":"PRIVATE"}]}`,
		`{"role":"user","model":"model","content":[{"type":"text","text":"PRIVATE"}]}`,
		`{"role":"assistant","model":"model","content":[{"type":"tool_use","name":"PRIVATE"}]}`,
		`{"role":"assistant","model":"model","content":[{"type":"text","text":null}]}`,
		`{"role":"assistant","model":"model","content":[]}`,
		`{"role":"assistant","model":"model","content":[{"type":"thinking","thinking":null}]}`,
	} {
		if text, err := firstAssistantText([]byte(body), "model"); err == nil || text != "" || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE") {
			t.Fatal("invalid or private response projected")
		}
	}
	block, _ := json.Marshal(map[string]any{"role": "assistant", "model": "model", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", maxFirstAnswerBytes+1)}}})
	if _, err := firstAssistantText(block, "model"); err == nil {
		t.Fatal("unbounded response")
	}
}

func TestFirstQueryCannotReuseNoTurnArchiveClaim(t *testing.T) {
	s := &Session{closed: make(chan struct{}), firstQuestion: &firstQuestionState{}, receipt: ReferenceReceipt{NoTurnAcknowledged: true}}
	close(s.closed)
	if _, err := s.VerifyArchive(context.Background(), "/unused"); !errors.Is(err, ErrState) {
		t.Fatal("queried session entered reference-only archive proof", err)
	}
	r := FirstExchangeResult{Answer: "PRIVATE_BODY"}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", r, r, r), "PRIVATE_BODY") {
		t.Fatal("response formatting leaked its body")
	}
}
