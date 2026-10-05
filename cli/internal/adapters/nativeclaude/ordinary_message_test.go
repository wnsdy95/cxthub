package nativeclaude

import (
	"encoding/json"
	"testing"
)

func TestOrdinaryAssistantProjectionBindsFinalization(t *testing.T) {
	wire := json.RawMessage(`{"id":"api-1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"answer"}],"stop_sequence":null,"stop_reason":null,"context_management":null,"usage":{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`)
	m, err := object(wire)
	if err != nil {
		t.Fatal(err)
	}
	want, output, err := ordinaryAssistantProjection(m, "")
	if err != nil || output != 1 {
		t.Fatal("wire projection", err)
	}
	delete(m, "context_management")
	m["stop_details"], m["stop_reason"] = json.RawMessage("null"), json.RawMessage(`"end_turn"`)
	m["usage"] = json.RawMessage(`{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"service_tier":"standard","speed":"standard","inference_geo":"","iterations":[],"fallback_credit":null}`)
	got, output, err := ordinaryAssistantProjection(m, "end_turn")
	if err != nil || got != want || output != 4 {
		t.Fatal("final accounting changed stable message", err)
	}
	base, _ := json.Marshal(m)
	for key, value := range map[string]string{
		"id": `"api-2"`, "model": `"other"`, "role": `"user"`, "type": `"other"`,
		"content": `[]`, "stop_sequence": `"different"`, "stop_reason": `"tool_use"`,
		"context_management": `{}`, "stop_details": `{}`, "unknown": `true`,
		"usage": `{"input_tokens":65,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}`,
	} {
		t.Run(key, func(t *testing.T) {
			changed, _ := object(base)
			changed[key] = json.RawMessage(value)
			hash, _, err := ordinaryAssistantProjection(changed, "end_turn")
			if err == nil && hash == want {
				t.Fatal("changed semantic field accepted")
			}
		})
	}
	for _, usage := range []string{
		`{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":-1}`,
		`{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1,"iterations":[{}]}`,
		`{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1,"output_tokens_details":{"thinking_tokens":null}}`,
		`{"input_tokens":64,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1,"unknown":true}`,
	} {
		changed, _ := object(base)
		changed["usage"] = json.RawMessage(usage)
		if _, _, err := ordinaryAssistantProjection(changed, "end_turn"); err == nil {
			t.Fatal("invalid final accounting accepted")
		}
	}
}
