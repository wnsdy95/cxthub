package nativeclaude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// All diagnostics are fixed adapter errors. Native error/account/body values
// are parsed only when necessary and never included in errors or receipts.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrProtocol
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, ErrProtocol
		}
		if _, found := out[name]; found {
			return nil, ErrProtocol
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrProtocol
		}
		out[name] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, ErrProtocol
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrProtocol
	}
	return out, nil
}

func stringField(m map[string]json.RawMessage, key string) (string, error) {
	var value string
	if json.Unmarshal(m[key], &value) != nil || value == "" || len(value) > 512 || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return "", ErrProtocol
	}
	return value, nil
}
func count(m map[string]json.RawMessage, key string, minimum int64) (int64, error) {
	raw, ok := m[key]
	var value int64
	if !ok || bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil || value < minimum {
		return 0, ErrProtocol
	}
	return value, nil
}
func boolean(m map[string]json.RawMessage, key string) (bool, error) {
	var b bool
	raw, ok := m[key]
	if !ok || bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &b) != nil {
		return false, ErrProtocol
	}
	return b, nil
}

func parseModels(raw []byte) ([]ModelInfo, error) {
	m, err := object(raw)
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if json.Unmarshal(m["models"], &rows) != nil || rows == nil || len(rows) > 100 {
		return nil, ErrProtocol
	}
	seen := map[string]bool{}
	models := make([]ModelInfo, 0, len(rows))
	for _, raw := range rows {
		row, err := object(raw)
		if err != nil {
			return nil, err
		}
		value, err := stringField(row, "value")
		if err != nil || seen[value] {
			return nil, ErrProtocol
		}
		seen[value] = true
		model := ModelInfo{Value: value}
		if _, ok := row["resolvedModel"]; ok {
			model.ResolvedModel, err = stringField(row, "resolvedModel")
			if err != nil {
				return nil, err
			}
		}
		models = append(models, model)
	}
	return models, nil
}

func parseSummary(raw []byte, id string) (ContextSummary, error) {
	s := ContextSummary{SessionID: id, Measurement: "local_estimate"}
	m, err := object(raw)
	if err != nil {
		return ContextSummary{}, err
	}
	if s.Model, err = stringField(m, "model"); err != nil {
		return ContextSummary{}, err
	}
	for key, ptr := range map[string]*int64{"totalTokens": &s.TotalTokens, "maxTokens": &s.MaxTokens, "rawMaxTokens": &s.RawMaxTokens} {
		minimum := int64(1)
		if key == "totalTokens" {
			minimum = 0
		}
		*ptr, err = count(m, key, minimum)
		if err != nil {
			return ContextSummary{}, err
		}
	}
	if s.AutoCompactEnabled, err = boolean(m, "isAutoCompactEnabled"); err != nil {
		return ContextSummary{}, err
	}
	if _, ok := m["autoCompactThreshold"]; ok {
		v, err := count(m, "autoCompactThreshold", 1)
		if err != nil {
			return ContextSummary{}, err
		}
		s.AutoCompactThreshold = &v
	}
	if !bytes.Equal(bytes.TrimSpace(m["apiUsage"]), []byte("null")) {
		return ContextSummary{}, ErrProtocol
	}
	return s, nil
}

func (s *Session) frame(raw []byte) error {
	m, err := object(raw)
	if err != nil {
		return err
	}
	kind, err := stringField(m, "type")
	if err != nil {
		return err
	}
	if session, ok := m["session_id"]; ok {
		var id string
		if json.Unmarshal(session, &id) != nil || id != s.id {
			return ErrProtocol
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.firstQuestion != nil {
		if handled, err := s.firstQuestionFrame(m, kind); handled {
			return err
		}
	}
	switch kind {
	case "control_response":
		if s.pending == nil || s.pending.kind == "append" || s.pending.delivered {
			return ErrProtocol
		}
		r, err := object(m["response"])
		if err != nil {
			return err
		}
		id, err := stringField(r, "request_id")
		if err != nil || id != s.pending.id {
			return ErrProtocol
		}
		status, err := stringField(r, "subtype")
		if err != nil || status != "success" {
			return ErrProtocol
		}
		for _, key := range []string{"pending_permission_requests", "pending_user_dialog_requests"} {
			if pending, ok := r[key]; ok {
				var values []json.RawMessage
				if json.Unmarshal(pending, &values) != nil || len(values) != 0 {
					return ErrProtocol
				}
			}
		}
		if _, err = object(r["response"]); err != nil {
			return err
		}
		s.pending.delivered = true
		s.pending.result <- r["response"]
		return nil
	case "user":
		if !s.appended || s.receipt.ReplayAcknowledged {
			return ErrProtocol
		}
		id, err := stringField(m, "uuid")
		if err != nil || id != s.receipt.MessageID {
			return ErrProtocol
		}
		id, err = stringField(m, "session_id")
		if err != nil || id != s.id {
			return ErrProtocol
		}
		if !bytes.Equal(bytes.TrimSpace(m["parent_tool_use_id"]), []byte("null")) {
			return ErrProtocol
		}
		text, err := referenceText(m["message"])
		if err != nil {
			return err
		}
		if len(text) != s.receipt.UTF8Bytes || hashText(text) != s.receipt.PayloadHash {
			return ErrProtocol
		}
		s.receipt.ReplayAcknowledged = true
		return nil
	case "command_lifecycle":
		id, err := stringField(m, "session_id")
		if err != nil || id != s.id || !s.appended {
			return ErrProtocol
		}
		id, err = stringField(m, "command_uuid")
		if err != nil || id != s.receipt.MessageID {
			return ErrProtocol
		}
		state, err := stringField(m, "state")
		if err != nil {
			return err
		}
		expected := []string{"queued", "started", "completed"}
		if s.appendPhase >= len(expected) || state != expected[s.appendPhase] {
			return ErrProtocol
		}
		if state == "completed" && !s.appendResult {
			return ErrProtocol
		}
		s.appendPhase++
		if state == "completed" {
			if s.pending == nil || s.pending.kind != "append" || s.pending.delivered {
				return ErrProtocol
			}
			s.receipt.NoTurnAcknowledged = true
			s.pending.delivered = true
			s.pending.result <- nil
		}
		return nil
	case "system":
		subtype, err := stringField(m, "subtype")
		if err != nil {
			return err
		}
		switch subtype {
		case "ui_invalidate", "informational":
			return s.validateHostNotification(m, subtype)
		case "init":
			if raw, ok := m["cwd"]; ok {
				var cwd string
				if json.Unmarshal(raw, &cwd) != nil || cwd != s.cwd {
					return fmt.Errorf("%w: init cwd", ErrProtocol)
				}
			}
			if raw, ok := m["claude_code_version"]; ok {
				var version string
				if json.Unmarshal(raw, &version) != nil || version != s.version {
					return fmt.Errorf("%w: init version", ErrProtocol)
				}
			}
		case "status":
			if raw, ok := m["status"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return ErrProtocol
			}
		case "commands_changed":
			if id, err := stringField(m, "session_id"); err != nil || id != s.id {
				return ErrProtocol
			}
			var commands []json.RawMessage
			if json.Unmarshal(m["commands"], &commands) != nil || commands == nil || len(commands) > 1000 {
				return ErrProtocol
			}
			for _, raw := range commands {
				command, err := object(raw)
				if err != nil {
					return err
				}
				if _, err := stringField(command, "name"); err != nil {
					return err
				}
				if raw, ok := command["description"]; ok {
					var description string
					if json.Unmarshal(raw, &description) != nil {
						return ErrProtocol
					}
				}
			}
		case "session_title_changed":
			if id, err := stringField(m, "session_id"); err != nil || id != s.id {
				return ErrProtocol
			}
			var title string
			if json.Unmarshal(m["title"], &title) != nil {
				return ErrProtocol
			}
		case "hook_started", "hook_progress", "hook_response":
			// Startup command-hook output can contain private bodies. It is
			// drained, never retained or interpreted as an input acknowledgment.
		default:
			return ErrProtocol
		}
		return nil
	case "result":
		if !s.appended || s.appendResult || s.appendPhase != 2 {
			return ErrProtocol
		}
		id, err := stringField(m, "session_id")
		if err != nil || id != s.id {
			return ErrProtocol
		}
		id, err = stringField(m, "user_message_uuid")
		if err != nil || id != s.receipt.MessageID {
			return ErrProtocol
		}
		if raw, ok := m["user_message_uuids"]; ok {
			var ids []string
			if json.Unmarshal(raw, &ids) != nil || len(ids) != 1 || ids[0] != id {
				return fmt.Errorf("%w: result message ids", ErrProtocol)
			}
		}
		if err := zeroTurnResult(m); err != nil {
			return err
		}
		index, err := count(m, "result_index", 0)
		if err != nil || index != 0 {
			return ErrProtocol
		}
		s.appendResult = true
		return nil
	default:
		// Includes assistant, stream_event, control_request, compact_boundary,
		// auth/permission requests and unknown protocol additions.
		return ErrProtocol
	}
}

func zeroTurnResult(m map[string]json.RawMessage) error {
	status, err := stringField(m, "subtype")
	if err != nil || status != "success" {
		return ErrProtocol
	}
	isError, err := boolean(m, "is_error")
	if err != nil || isError {
		return ErrProtocol
	}
	for _, key := range []string{"num_turns", "duration_api_ms"} {
		v, err := count(m, key, 0)
		if err != nil || v != 0 {
			return fmt.Errorf("%w: zero-turn timing/count", ErrProtocol)
		}
	}
	if raw, ok := m["total_cost_usd"]; ok {
		var cost float64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &cost) != nil || cost != 0 {
			return fmt.Errorf("%w: zero-turn cost", ErrProtocol)
		}
	}
	if _, ok := m["queued_turn_count"]; ok {
		n, err := count(m, "queued_turn_count", 0)
		if err != nil || n != 0 {
			return fmt.Errorf("%w: zero-turn queue", ErrProtocol)
		}
	}
	if raw, ok := m["permission_denials"]; ok {
		var denials []json.RawMessage
		if json.Unmarshal(raw, &denials) != nil || len(denials) != 0 {
			return fmt.Errorf("%w: zero-turn permission denials", ErrProtocol)
		}
	}
	usage, err := object(m["usage"])
	if err != nil {
		return err
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		v, err := count(usage, key, 0)
		if err != nil || v != 0 {
			return fmt.Errorf("%w: zero-turn usage", ErrProtocol)
		}
	}
	for key, raw := range usage {
		switch key {
		case "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens":
			// Required scalar counts validated above.
		case "cache_creation", "output_tokens_details", "server_tool_use":
			if err := zeroCountDetails(raw); err != nil {
				return err
			}
		case "iterations":
			var iterations []json.RawMessage
			if json.Unmarshal(raw, &iterations) != nil || iterations == nil || len(iterations) != 0 {
				return ErrProtocol
			}
		case "inference_geo", "service_tier", "speed":
			var metadata string
			if json.Unmarshal(raw, &metadata) != nil {
				return ErrProtocol
			}
		case "fallback_credit":
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return ErrProtocol
			}
		default:
			return fmt.Errorf("%w: unsupported usage field", ErrProtocol)
		}
	}
	if raw, ok := m["modelUsage"]; ok {
		rows, err := object(raw)
		if err != nil || len(rows) != 0 {
			return ErrProtocol
		}
	}
	return nil
}

// In the pinned protocol output_tokens_details is an object, not a token count. Require
// every supplied detail (including cache durations and server tools) to be a
// typed zero count; do not ignore nonzero usage nested under these objects.
func zeroCountDetails(raw []byte) error {
	details, err := object(raw)
	if err != nil {
		return err
	}
	for key := range details {
		n, err := count(details, key, 0)
		if err != nil || n != 0 {
			return fmt.Errorf("%w: nonzero or malformed usage detail", ErrProtocol)
		}
	}
	return nil
}

func referenceText(raw []byte) (string, error) {
	m, err := object(raw)
	if err != nil {
		return "", err
	}
	role, err := stringField(m, "role")
	if err != nil || role != "user" {
		return "", ErrProtocol
	}
	var text string
	if json.Unmarshal(m["content"], &text) == nil {
		return text, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(m["content"], &blocks) != nil || len(blocks) != 1 {
		return "", ErrProtocol
	}
	b, err := object(blocks[0])
	if err != nil {
		return "", err
	}
	kind, err := stringField(b, "type")
	if err != nil || kind != "text" || json.Unmarshal(b["text"], &text) != nil {
		return "", ErrProtocol
	}
	return text, nil
}
