package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
)

const maxOrdinaryRecords = 128

type ordinaryTool struct {
	name, assistant, messageID string
	done                       bool
}
type ordinaryRecord struct {
	id, kind, messageID, contentHash, messageHash string
	toolIDs                                       []string
	// Native tool metadata participates in replay and is bound independently
	// from the model-visible content. Only hashes survive the wire reducer.
	toolResultHash string
	outputTokens   int64
	terminalStop   string
}
type ordinaryState struct {
	ctx           context.Context
	cancel        context.CancelFunc
	handlers      InteractionHandlers
	controls      map[string]*ordinaryControl
	tools         map[string]*ordinaryTool
	messageIDs    map[string]bool
	recordIDs     map[string]bool
	records       []ordinaryRecord
	questionHash  string
	questionBytes int
	progress      int
}

func newOrdinaryState(ctx context.Context, handlers InteractionHandlers) *ordinaryState {
	ctx, cancel := context.WithCancel(ctx)
	return &ordinaryState{ctx: ctx, cancel: cancel, handlers: handlers, controls: map[string]*ordinaryControl{}, tools: map[string]*ordinaryTool{}, messageIDs: map[string]bool{}, recordIDs: map[string]bool{}}
}
func (o *ordinaryState) settled() bool {
	for _, c := range o.controls {
		if !c.settled {
			return false
		}
	}
	for _, t := range o.tools {
		if !t.done {
			return false
		}
	}
	return true
}

// Called only with Session.mu held. Common lifecycle/result and informational
// handling stays in the common firstQuestionFrame reducer.
func (s *Session) ordinaryFrame(m map[string]json.RawMessage, kind string) (bool, error) {
	q, o := s.firstQuestion, s.firstQuestion.ordinary
	switch kind {
	case "control_request", "control_cancel_request":
		return true, s.ordinaryControl(m, kind)
	case "control_response":
		return true, s.ordinaryControlEcho(m)
	case "user", "assistant", "tool_progress":
	default:
		return false, nil
	}
	if id, err := stringField(m, "session_id"); err != nil || id != s.id || q.completed || q.resultReceived || s.pending == nil || s.pending.kind != "first_question" || s.pending.id != q.id || s.pending.delivered {
		return true, ErrProtocol
	}
	if !bytes.Equal(bytes.TrimSpace(m["parent_tool_use_id"]), []byte("null")) {
		return true, ErrUnsupported
	}
	if err := ordinaryBehavior(m); err != nil {
		return true, err
	}
	if kind == "tool_progress" {
		if q.phase != 2 || o.progress >= 1024 {
			return true, ErrLimit
		}
		id, err := stringField(m, "tool_use_id")
		name, nameErr := stringField(m, "tool_name")
		tool := o.tools[id]
		if err != nil || nameErr != nil || tool == nil || tool.done || tool.name != name {
			return true, ErrProtocol
		}
		if _, err := exchangeNumber(m["elapsed_time_seconds"], exchangeNativeNumberMax); err != nil {
			return true, err
		}
		o.progress++
		return true, nil
	}
	id, err := stringField(m, "uuid")
	if err != nil || o.recordIDs[id] || len(o.records) >= maxOrdinaryRecords {
		return true, ErrProtocol
	}
	if kind == "user" && id == q.id {
		if q.replayed || q.phase > 2 {
			return true, ErrProtocol
		}
		for key, want := range map[string]bool{"isSynthetic": false, "client_composed": false, "shouldQuery": true} {
			if _, ok := m[key]; ok {
				b, err := boolean(m, key)
				if err != nil || b != want {
					return true, ErrProtocol
				}
			}
		}
		text, err := referenceText(m["message"])
		if err != nil || text == "" || len(text) > MaxReferenceBytes {
			return true, ErrProtocol
		}
		// Native hooks may rewrite the admitted input. Bind that observed
		// projection by root UUID without mislabelling it as submitted text.
		o.questionHash, o.questionBytes = hashText(text), len(text)
		q.replayed = true
		o.recordIDs[id] = true
		return true, nil
	}
	if q.phase != 2 {
		return true, ErrProtocol
	}
	message, err := object(m["message"])
	if err != nil {
		return true, err
	}
	role, err := stringField(message, "role")
	if err != nil || role != kind {
		return true, ErrProtocol
	}
	if _, ok := m["user_message_uuid"]; ok || kind == "assistant" && len(q.assistantIDs) == 0 {
		root, err := stringField(m, "user_message_uuid")
		if err != nil || root != q.id {
			return true, ErrProtocol
		}
	}
	if raw, ok := m["user_message_uuids"]; ok {
		var ids []string
		if json.Unmarshal(raw, &ids) != nil || len(ids) != 1 || ids[0] != q.id {
			return true, ErrProtocol
		}
	}
	record := ordinaryRecord{id: id, kind: kind}
	record.contentHash, err = archiveContentHash(message["content"])
	if err != nil {
		return true, err
	}
	if kind == "assistant" {
		record.messageHash, record.outputTokens, err = ordinaryAssistantProjection(message, "")
		if err != nil {
			return true, err
		}
		if raw, ok := message["stop_reason"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			record.terminalStop, _ = stringField(message, "stop_reason")
		}
		messageID, err := stringField(message, "id")
		if err != nil || q.messageID != messageID && o.messageIDs[messageID] {
			return true, ErrProtocol
		}
		if q.messageID != "" && q.messageID != messageID && !o.settled() {
			return true, ErrProtocol
		}
		text, last, tools, err := ordinaryAssistant(message, q.summary.Model)
		if err != nil {
			return true, err
		}
		if len(text) > maxFirstAnswerBytes-len(q.assistantText) {
			return true, ErrLimit
		}
		for toolID, tool := range tools {
			if o.tools[toolID] != nil || len(o.tools) >= 64 {
				return true, ErrProtocol
			}
			tool.assistant, tool.messageID = id, messageID
			o.tools[toolID] = tool
		}
		o.messageIDs[messageID] = true
		q.messageID = messageID
		record.messageID = messageID
		q.assistantText += text
		q.lastText = last
		if q.assistantIDs == nil {
			q.assistantIDs = map[string]bool{}
		}
		q.assistantIDs[id] = true
		q.archiveMessages = append(q.archiveMessages, archiveAssistant{id: id, contentHash: record.contentHash})
	} else {
		record.messageHash, err = archiveContentHash(m["message"])
		if err != nil {
			return true, err
		}
		ids, err := ordinaryToolResults(message["content"])
		if err != nil {
			return true, err
		}
		group := ""
		for _, toolID := range ids {
			t := o.tools[toolID]
			if t == nil || t.done {
				return true, ErrProtocol
			}
			if group != "" && group != t.messageID {
				return true, ErrProtocol
			}
			group = t.messageID
			for _, c := range o.controls {
				if c.toolID == toolID && !c.settled {
					return true, ErrProtocol
				}
			}
			t.done = true
		}
		record.toolIDs = ids
		if raw, ok := m["tool_use_result"]; ok {
			record.toolResultHash, err = archiveContentHash(raw)
			if err != nil {
				return true, err
			}
		}
	}
	o.recordIDs[id] = true
	o.records = append(o.records, record)
	return true, nil
}

func ordinaryBehavior(m map[string]json.RawMessage) error {
	for _, key := range []string{"is_api_error_message", "is_virtual", "isApiErrorMessage", "isVisibleInTranscriptOnly", "isMeta", "isCompactSummary"} {
		if raw, ok := m[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			return ErrUnsupported
		}
	}
	for _, key := range []string{"error", "supersedes", "modelOnlyText", "replacesSpan", "ephemeral", "isCompactSummaryBefore", "summarizeMetadata", "toolDenialEndsTurn", "toolEndsTurn", "interruptedMessageId", "interruptedByShutdown", "promptShellHandOff"} {
		if _, ok := m[key]; ok {
			return ErrUnsupported
		}
	}
	return nil
}

func ordinaryAssistant(m map[string]json.RawMessage, model string) (text, last string, tools map[string]*ordinaryTool, err error) {
	tools = map[string]*ordinaryTool{}
	if role, e := stringField(m, "role"); e != nil || role != "assistant" {
		err = ErrProtocol
		return
	}
	if actual, e := stringField(m, "model"); e != nil || actual != model {
		err = ErrProtocol
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(m["content"], &blocks) != nil || len(blocks) == 0 || len(blocks) > 64 {
		err = ErrProtocol
		return
	}
	for _, raw := range blocks {
		b, e := object(raw)
		if e != nil {
			err = e
			return
		}
		kind, e := stringField(b, "type")
		if e != nil {
			err = e
			return
		}
		last = ""
		if kind == "tool_use" {
			if !exchangeKeys(b, "type", "id", "name", "input", "caller") {
				err = ErrUnsupported
				return
			}
			// Server-side tool callers and subagent execution need independent
			// authority/correlation. Do not silently flatten them into root tools.
			if _, ok := b["caller"]; ok {
				err = ErrUnsupported
				return
			}
			id, e := stringField(b, "id")
			if e != nil || tools[id] != nil {
				err = ErrProtocol
				return
			}
			name, e := stringField(b, "name")
			if e != nil {
				err = e
				return
			}
			if len(b["input"]) > maxPermissionInput {
				err = ErrLimit
				return
			}
			if _, e := object(b["input"]); e != nil {
				err = e
				return
			}
			tools[id] = &ordinaryTool{name: name}
			continue
		}
		value, e := assistantTextBlock(b, kind)
		if e != nil {
			err = ErrUnsupported
			return
		}
		if len(value) > maxFirstAnswerBytes-len(text) {
			err = ErrLimit
			return
		}
		text += value
		if kind == "text" {
			last = value
		}
	}
	return
}

func ordinaryToolResults(raw json.RawMessage) ([]string, error) {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 || len(blocks) > 64 {
		return nil, ErrProtocol
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, raw := range blocks {
		b, err := object(raw)
		if err != nil || !exchangeKeys(b, "type", "tool_use_id", "content", "is_error") {
			return nil, ErrUnsupported
		}
		kind, err := stringField(b, "type")
		if err != nil || kind != "tool_result" {
			return nil, ErrUnsupported
		}
		id, err := stringField(b, "tool_use_id")
		if err != nil || seen[id] {
			return nil, ErrProtocol
		}
		seen[id] = true
		if raw, ok := b["is_error"]; ok {
			if _, err := exchangeBool(raw); err != nil {
				return nil, err
			}
		}
		if _, err := exchangeString(b["content"], maxFirstAnswerBytes); err != nil {
			var content []json.RawMessage
			if json.Unmarshal(b["content"], &content) != nil || content == nil || len(content) > 64 {
				return nil, ErrUnsupported
			}
			for _, raw := range content {
				m, err := object(raw)
				if err != nil || !exchangeKeys(m, "type", "text") {
					return nil, ErrUnsupported
				}
				kind, err := stringField(m, "type")
				if err != nil || kind != "text" {
					return nil, ErrUnsupported
				}
				if _, err := exchangeString(m["text"], maxFirstAnswerBytes); err != nil {
					return nil, err
				}
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (o *ordinaryState) validateDenials(rows []json.RawMessage) error {
	if len(rows) > len(o.tools) {
		return ErrProtocol
	}
	seen := map[string]bool{}
	for _, raw := range rows {
		m, err := object(raw)
		if err != nil {
			return err
		}
		id, err := stringField(m, "tool_use_id")
		if err != nil {
			return err
		}
		name, err := stringField(m, "tool_name")
		if err != nil {
			return err
		}
		t := o.tools[id]
		if t == nil || !t.done || t.name != name || seen[id] {
			return ErrProtocol
		}
		seen[id] = true
		if raw, ok := m["tool_input"]; ok {
			if _, err := object(raw); err != nil {
				return err
			}
		}
	}
	return nil
}
