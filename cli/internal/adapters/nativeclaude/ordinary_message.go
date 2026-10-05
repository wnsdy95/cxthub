package nativeclaude

import (
	"bytes"
	"encoding/json"
)

// The SDK emits an assistant block before its API message finishes. The
// archive finalizes stop_reason/output usage and adds accounting fields. Bind
// stable identity, content and input accounting across both representations;
// separately validate final metadata instead of requiring an impossible whole
// message hash match. The full archive is still bound by its own file hash.
func ordinaryAssistantProjection(message map[string]json.RawMessage, finalStop string) (string, int64, error) {
	if !exchangeKeys(message, "id", "type", "role", "model", "content", "stop_sequence", "stop_reason", "context_management", "stop_details", "usage") {
		return "", 0, ErrUnsupported
	}
	stable := make(map[string]json.RawMessage, len(message))
	for key, raw := range message {
		stable[key] = raw
	}
	for _, key := range []string{"context_management", "stop_details"} {
		if raw, ok := message[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return "", 0, ErrUnsupported
		}
		delete(stable, key)
	}
	if raw, ok := message["stop_reason"]; ok {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			stop, err := exchangeString(raw, 32)
			if err != nil || (stop != "tool_use" && stop != "end_turn") || (finalStop != "" && stop != finalStop) {
				return "", 0, ErrProtocol
			}
		} else if finalStop != "" {
			return "", 0, ErrProtocol
		}
		stable["stop_reason"] = json.RawMessage("null")
	}
	var output int64
	if raw, ok := message["usage"]; ok {
		usage, err := object(raw)
		if err != nil || !exchangeKeys(usage, "input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens", "output_tokens_details", "server_tool_use", "service_tier", "cache_creation", "inference_geo", "iterations", "speed", "fallback_credit") {
			return "", 0, ErrProtocol
		}
		input := map[string]json.RawMessage{}
		for _, key := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"} {
			n, err := count(usage, key, 0)
			if err != nil || n > int64(exchangeNativeNumberMax) {
				return "", 0, ErrProtocol
			}
			if key == "output_tokens" {
				output = n
			} else {
				input[key] = usage[key]
			}
		}
		if err := ordinaryUsageDetails(usage); err != nil {
			return "", 0, err
		}
		stable["usage"], err = json.Marshal(input)
		if err != nil {
			return "", 0, ErrProtocol
		}
	}
	raw, err := json.Marshal(stable)
	if err != nil {
		return "", 0, ErrProtocol
	}
	hash, err := archiveContentHash(raw)
	return hash, output, err
}

func ordinaryUsageDetails(usage map[string]json.RawMessage) error {
	for key, keys := range map[string][]string{
		"output_tokens_details": {"thinking_tokens"},
		"server_tool_use":       {"web_search_requests", "web_fetch_requests"},
		"cache_creation":        {"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"},
	} {
		if raw, ok := usage[key]; ok {
			fields, err := object(raw)
			if err != nil || !exchangeKeys(fields, keys...) {
				return ErrProtocol
			}
			for _, field := range keys {
				n, err := count(fields, field, 0)
				if err != nil || n > int64(exchangeNativeNumberMax) {
					return ErrProtocol
				}
			}
		}
	}
	for _, key := range []string{"service_tier", "inference_geo", "speed"} {
		if raw, ok := usage[key]; ok {
			if _, err := exchangeString(raw, 128); err != nil {
				return err
			}
		}
	}
	if raw, ok := usage["iterations"]; ok {
		var rows []json.RawMessage
		if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) != 0 {
			return ErrUnsupported
		}
	}
	if raw, ok := usage["fallback_credit"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrUnsupported
	}
	return nil
}
