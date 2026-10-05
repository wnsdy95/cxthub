package nativeclaude

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

const maxFirstAnswerBytes = 1 << 20

type firstQuestionState struct {
	id, hash        string
	bytes           int
	summary         ContextSummary
	phase           int
	replayed        bool
	resultReceived  bool
	completed       bool
	assistantIDs    map[string]bool
	archiveMessages []archiveAssistant
	messageID       string
	assistantText   string
	lastText        string
	answer          string
	ordinary        *ordinaryState
}

// Caller holds Session.mu. Only an explicitly owned first question may receive
// querying frames. Ordinary permission callbacks are dispatched separately;
// this reader never executes caller code or waits for an interaction.
func (s *Session) firstQuestionFrame(m map[string]json.RawMessage, kind string) (bool, error) {
	q := s.firstQuestion
	if q.ordinary != nil {
		if handled, err := s.ordinaryFrame(m, kind); handled {
			return true, err
		}
	}
	switch kind {
	case "rate_limit_event":
		return true, s.validateRateLimitNotification(m)
	case "control_request":
		return true, ErrPermissionRequired
	case "control_response":
		return true, ErrProtocol
	case "user", "assistant", "result", "command_lifecycle":
		id, err := stringField(m, "session_id")
		if err != nil || id != s.id || q.completed || s.pending == nil || s.pending.kind != "first_question" || s.pending.id != q.id || s.pending.delivered {
			return true, ErrProtocol
		}
	case "system":
		if bytes.Equal(m["subtype"], []byte(`"thinking_tokens"`)) {
			return true, s.validateThinkingProgress(m)
		}
		if q.ordinary != nil && bytes.Equal(m["subtype"], []byte(`"compact_boundary"`)) {
			return true, ErrUnsupported
		}
		if subtype, err := stringField(m, "subtype"); err == nil && subtype == "status" {
			id, err := stringField(m, "session_id")
			if err != nil || id != s.id {
				return true, ErrProtocol
			}
			status := bytes.TrimSpace(m["status"])
			if bytes.Equal(status, []byte("null")) {
				return true, nil
			}
			var value string
			if q.phase != 2 || q.resultReceived || json.Unmarshal(status, &value) != nil || value != "requesting" {
				return true, ErrProtocol
			}
			return true, nil
		}
		if raw, ok := m["model"]; ok {
			var model string
			if json.Unmarshal(raw, &model) != nil || model != q.summary.Model {
				return true, ErrProtocol
			}
		}
		// Existing metadata validation still rejects compaction and non-null
		// status; the no-query lifecycle never treats metadata as admission.
		return false, nil
	default:
		return true, ErrProtocol
	}
	switch kind {
	case "command_lifecycle":
		id, err := stringField(m, "command_uuid")
		if err != nil || id != q.id {
			return true, ErrProtocol
		}
		state, err := stringField(m, "state")
		if q.ordinary != nil && (state == "cancelled" || state == "discarded" || state == "refused") {
			return true, ErrUnsupported
		}
		want := []string{"queued", "started", "completed"}
		if err != nil || q.phase >= len(want) || state != want[q.phase] || state == "completed" && !q.resultReceived {
			return true, ErrProtocol
		}
		q.phase++
		if state == "completed" {
			q.completed = true
			s.pending.delivered = true
			s.pending.result <- nil
		}
	case "user":
		id, err := stringField(m, "uuid")
		if err != nil || id != q.id || q.replayed || q.resultReceived || !bytes.Equal(bytes.TrimSpace(m["parent_tool_use_id"]), []byte("null")) {
			return true, ErrProtocol
		}
		for key, want := range map[string]bool{"isSynthetic": false, "shouldQuery": true, "client_composed": true} {
			if _, ok := m[key]; ok {
				value, err := boolean(m, key)
				if err != nil || value != want {
					return true, ErrProtocol
				}
			}
		}
		text, err := referenceText(m["message"])
		if err != nil || len(text) != q.bytes || hashText(text) != q.hash {
			return true, ErrProtocol
		}
		q.replayed = true
	case "assistant":
		if _, hasError := m["error"]; hasError {
			return true, ErrProtocol
		}
		if q.phase != 2 || q.resultReceived || !bytes.Equal(bytes.TrimSpace(m["parent_tool_use_id"]), []byte("null")) {
			return true, ErrProtocol
		}
		id, err := stringField(m, "uuid")
		if err != nil || len(q.assistantIDs) >= 64 || q.assistantIDs[id] {
			return true, ErrProtocol
		}
		if _, ok := m["user_message_uuid"]; ok || len(q.assistantIDs) == 0 {
			userID, err := stringField(m, "user_message_uuid")
			if err != nil || userID != q.id {
				return true, ErrProtocol
			}
		}
		if raw, ok := m["user_message_uuids"]; ok {
			var ids []string
			if json.Unmarshal(raw, &ids) != nil || len(ids) != 1 || ids[0] != q.id {
				return true, ErrProtocol
			}
		}
		message, err := object(m["message"])
		if err != nil {
			return true, err
		}
		messageID, err := stringField(message, "id")
		if err != nil || q.messageID != "" && q.messageID != messageID {
			return true, ErrProtocol
		}
		q.messageID = messageID
		text, err := firstAssistantText(m["message"], q.summary.Model)
		if err != nil || len(text) > maxFirstAnswerBytes-len(q.assistantText) {
			return true, ErrProtocol
		}
		// Native's result projects the last block of the latest assistant
		// record, not all visible text. Keep the full visible response separate.
		var blocks []json.RawMessage
		_ = json.Unmarshal(message["content"], &blocks) // validated above
		last, _ := object(blocks[len(blocks)-1])
		q.lastText = ""
		if kind, _ := stringField(last, "type"); kind == "text" {
			_ = json.Unmarshal(last["text"], &q.lastText)
		}
		if q.assistantIDs == nil {
			q.assistantIDs = map[string]bool{}
		}
		contentHash, err := archiveContentHash(message["content"])
		if err != nil {
			return true, err
		}
		q.archiveMessages = append(q.archiveMessages, archiveAssistant{id: id, contentHash: contentHash})
		q.assistantIDs[id] = true
		q.assistantText += text
	case "result":
		if q.phase != 2 || q.resultReceived || !q.replayed || len(q.assistantIDs) == 0 {
			return true, ErrProtocol
		}
		if q.ordinary != nil && !q.ordinary.settled() {
			return true, ErrProtocol
		}
		answer, err := firstQuestionResult(m, q)
		if err != nil && q.ordinary != nil {
			return true, err
		}
		if err != nil || answer != q.lastText {
			return true, ErrProtocol
		}
		q.answer, q.resultReceived = q.assistantText, true
	}
	return true, nil
}

// Pinned 2.1.287 emits numerical progress before an assistant record, including
// during redacted thinking. It is neither response content, billable usage nor
// completion evidence. Drain only progress correlated to the active question;
// no-query sessions must continue rejecting it. Totals reset per thinking block.
func (s *Session) validateThinkingProgress(m map[string]json.RawMessage) error {
	q := s.firstQuestion
	if s.version != "2.1.287" || q == nil || q.phase != 2 || q.completed || q.resultReceived ||
		s.pending == nil || s.pending.kind != "first_question" || s.pending.id != q.id || s.pending.delivered ||
		!exchangeKeys(m, "type", "subtype", "session_id", "uuid", "user_message_uuid", "estimated_tokens", "estimated_tokens_delta") {
		return ErrProtocol
	}
	if id, err := stringField(m, "session_id"); err != nil || id != s.id {
		return ErrProtocol
	}
	if id, err := stringField(m, "user_message_uuid"); err != nil || id != q.id {
		return ErrProtocol
	}
	if _, err := stringField(m, "uuid"); err != nil {
		return ErrProtocol
	}
	total, err := count(m, "estimated_tokens", 0)
	if err != nil || total > int64(exchangeNativeNumberMax) {
		return ErrProtocol
	}
	delta, err := count(m, "estimated_tokens_delta", 0)
	if err != nil || delta > total {
		return ErrProtocol
	}
	return nil
}

func firstAssistantText(raw []byte, model string) (string, error) {
	m, err := object(raw)
	if err != nil {
		return "", err
	}
	role, err := stringField(m, "role")
	if err != nil || role != "assistant" {
		return "", ErrProtocol
	}
	actual, err := stringField(m, "model")
	if err != nil || actual != model {
		return "", ErrProtocol
	}
	var blocks []json.RawMessage
	if json.Unmarshal(m["content"], &blocks) != nil || len(blocks) == 0 || len(blocks) > 64 {
		return "", ErrProtocol
	}
	var text strings.Builder
	for _, raw := range blocks {
		b, err := object(raw)
		if err != nil {
			return "", err
		}
		kind, err := stringField(b, "type")
		if err != nil {
			return "", err
		}
		value, err := assistantTextBlock(b, kind)
		if err != nil {
			return "", err
		}
		if len(value) > maxFirstAnswerBytes-text.Len() {
			return "", ErrLimit
		}
		text.WriteString(value)
	}
	return text.String(), nil
}

// Both protocols validate hidden blocks but project only visible text. Tool
// blocks remain the responsibility of the ordinary interaction protocol.
func assistantTextBlock(b map[string]json.RawMessage, kind string) (string, error) {
	key := ""
	switch kind {
	case "text":
		key = "text"
	case "thinking":
		key = "thinking"
	case "redacted_thinking":
		key = "data"
	default:
		return "", ErrProtocol
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(b[key]), []byte("null")) || json.Unmarshal(b[key], &value) != nil || !utf8.ValidString(value) || len(value) > maxFirstAnswerBytes {
		return "", ErrProtocol
	}
	if kind != "text" {
		return "", nil
	}
	return value, nil
}

func firstQuestionResult(m map[string]json.RawMessage, q *firstQuestionState) (string, error) {
	terminal, err := stringField(m, "terminal_reason")
	if err != nil || terminal != "completed" {
		if err == nil && q.ordinary != nil {
			return "", ErrUnsupported
		}
		return "", ErrProtocol
	}
	stop, err := stringField(m, "stop_reason")
	if err != nil || stop != "end_turn" {
		return "", ErrProtocol
	}
	index, err := count(m, "result_index", 0)
	if err != nil || index != 1 {
		return "", ErrProtocol
	}
	id, err := stringField(m, "user_message_uuid")
	if err != nil || id != q.id {
		return "", ErrProtocol
	}
	if raw, ok := m["user_message_uuids"]; ok {
		var ids []string
		if json.Unmarshal(raw, &ids) != nil || len(ids) != 1 || ids[0] != id {
			return "", ErrProtocol
		}
	}
	subtype, err := stringField(m, "subtype")
	if err != nil || subtype != "success" {
		return "", ErrProtocol
	}
	isError, err := boolean(m, "is_error")
	if err != nil || isError {
		return "", ErrProtocol
	}
	turns, err := count(m, "num_turns", 1)
	if err != nil || q.ordinary == nil && turns != 1 || q.ordinary != nil && turns > maxOrdinaryRecords {
		return "", ErrProtocol
	}
	if _, err = count(m, "duration_api_ms", 0); err != nil {
		return "", err
	}
	if raw, ok := m["total_cost_usd"]; ok {
		var cost float64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &cost) != nil || math.IsInf(cost, 0) || cost < 0 {
			return "", ErrProtocol
		}
	}
	if _, ok := m["queued_turn_count"]; ok {
		queued, err := count(m, "queued_turn_count", 0)
		if err != nil || queued != 0 {
			return "", ErrProtocol
		}
	}
	if raw, ok := m["permission_denials"]; ok {
		var denials []json.RawMessage
		if json.Unmarshal(raw, &denials) != nil || denials == nil || q.ordinary == nil && len(denials) != 0 {
			return "", ErrProtocol
		}
		if q.ordinary != nil {
			if err := q.ordinary.validateDenials(denials); err != nil {
				return "", err
			}
		}
	}
	usage, err := object(m["usage"])
	if err != nil {
		return "", err
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		minimum := int64(0)
		if key == "output_tokens" {
			minimum = 1
		}
		if _, err := count(usage, key, minimum); err != nil {
			return "", err
		}
	}
	models, err := object(m["modelUsage"])
	if err != nil || len(models) != 1 {
		return "", ErrProtocol
	}
	modelUsage, err := object(models[q.summary.Model])
	if err != nil {
		return "", err
	}
	if q.ordinary != nil {
		for _, key := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens", "webSearchRequests", "contextWindow", "maxOutputTokens"} {
			if _, ok := modelUsage[key]; ok {
				if _, err := count(modelUsage, key, 0); err != nil {
					return "", err
				}
			}
		}
		if raw, ok := modelUsage["costUSD"]; ok {
			if _, err := exchangeNumber(raw, exchangeNativeTotalCostMax); err != nil {
				return "", err
			}
		}
	}
	var answer string
	if json.Unmarshal(m["result"], &answer) != nil || answer == "" || !utf8.ValidString(answer) || len(answer) > maxFirstAnswerBytes {
		return "", ErrProtocol
	}
	return answer, nil
}
