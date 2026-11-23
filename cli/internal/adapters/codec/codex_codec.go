package codec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// CodexCodec implements ProviderCodec for Codex rollout JSONL ↔ CIR conversion (provider compatibility rules).
//
// Decode mapping (response_item.payload → CIR event):
//   - message{role,content:[{input_text|output_text,text}]} → message(role, blocks:[{text}...])
//   - function_call{name,arguments(JSON string|object),call_id} → tool_call(call_id, provider_tool_name:name, input:parse(arguments))
//   - function_call_output{call_id,output}                   → tool_result
//   - custom_tool_call{status,call_id,name,input(string)}    → tool_call(status, input:parse(input))
//   - custom_tool_call_output{call_id,output}                → tool_result
//   - web_search_call{action:{query}}                        → tool_call(tool_name:web_search, input:{query})
//   - reasoning{encrypted_content,summary}                    → reasoning(locked{scheme:encrypted_content}, redacted_summary=plaintext summary, cross_replayable:false)
//   - session_meta → envelope. turn_context{model} → envelope models (last wins, while accumulating all models).
//   - event_msg{token_count} → envelope token statistics (context=last_token_usage.input, output=last cumulative total). Other event_msg records are dropped.
//   - rollout records contain no git_branch; the calling app augments it through gitctx, so the codec leaves it empty.
//
// Encode(target=codex): full — reinject the original encrypted_content (FidelityFull).
// Encode(target!=codex): reconstructed — keep locked payloads inert and reverse-map tool_name.
// Encoding emits one session_meta line and one response_item line per event. Valid Codex-native events
// round-trip with equivalent meaning. A tool_result output incompatible with the target schema is normalized
// to a string for API safety, so structural identity is not guaranteed for arbitrary contaminated CIR.
type CodexCodec struct{}

// NewCodexCodec creates a CodexCodec.
func NewCodexCodec() *CodexCodec { return &CodexCodec{} }

// codexLine is a single line from a rollout JSONL: {timestamp, type, payload}.
type codexLine struct {
	Timestamp string          `json:"timestamp,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSessionMetaOut is the session_meta payload for encoding — codex 0.143 TUI resume
// parser schema mirror. Note (confirmed by binary search): non-standard key ("model") mixed
// with new schema fields (session_id/originator/...) causes schema union matching to fail in codex,
// resulting in "No saved session found" even if indexed. Separate from decode codexSessionMeta
// for maintenance (decode is conventional, encode follows real schema).
type codexSessionMetaOut struct {
	SessionID        string      `json:"session_id"`
	ID               string      `json:"id"`
	Timestamp        string      `json:"timestamp,omitempty"`
	Cwd              string      `json:"cwd"`
	Originator       string      `json:"originator"`
	CLIVersion       string      `json:"cli_version"`
	Source           string      `json:"source"`
	ThreadSource     string      `json:"thread_source"`
	Git              interface{} `json:"git"`
	MemoryMode       interface{} `json:"memory_mode"`
	BaseInstructions interface{} `json:"base_instructions"`
	ModelProvider    string      `json:"model_provider"`
}

type codexContent struct {
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

func (c codexContent) MarshalJSON() ([]byte, error) {
	m := map[string]string{"type": c.Type}
	if c.Type == "encrypted_content" {
		m["encrypted_content"] = c.EncryptedContent
	} else {
		// input_text/output_text require the text member even when its value is
		// empty. A struct-level omitempty cannot express this union correctly.
		m["text"] = c.Text
	}
	return json.Marshal(m)
}

type codexAction struct {
	Type  string `json:"type,omitempty"`
	Query string `json:"query,omitempty"`
}

// codexTokenUsage is the token usage from a token_count event (event_msg payload.info.*).
type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens,omitempty"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens,omitempty"`
	TotalTokens           int `json:"total_tokens,omitempty"`
}

// codexTokenInfo is the info field from a token_count event.
// total_token_usage is cumulative for the session, last_token_usage is for the previous request (context size at that point).
type codexTokenInfo struct {
	TotalTokenUsage *codexTokenUsage `json:"total_token_usage,omitempty"`
	LastTokenUsage  *codexTokenUsage `json:"last_token_usage,omitempty"`
}

// codexEventMsg is the portion of event_msg payload we use (token_count).
type codexEventMsg struct {
	Type    string          `json:"type"`
	Message string          `json:"message,omitempty"`
	Info    *codexTokenInfo `json:"info,omitempty"`
}

type codexResponseItem struct {
	Type             string                   `json:"type"`
	ID               string                   `json:"id,omitempty"`
	Role             string                   `json:"role,omitempty"`
	Content          []codexContent           `json:"content,omitempty"`
	Name             string                   `json:"name,omitempty"`
	Arguments        json.RawMessage          `json:"arguments,omitempty"`
	CallID           string                   `json:"call_id,omitempty"`
	Output           json.RawMessage          `json:"output,omitempty"`
	Input            string                   `json:"input,omitempty"`
	Status           string                   `json:"status,omitempty"`
	EncryptedContent string                   `json:"encrypted_content,omitempty"`
	Summary          []interface{}            `json:"summary,omitempty"`
	Action           *codexAction             `json:"action,omitempty"`
	Author           string                   `json:"author,omitempty"`
	Recipient        string                   `json:"recipient,omitempty"`
	ProviderMetadata *domain.ProviderMetadata `json:"internal_chat_message_metadata_passthrough,omitempty"`
}

type codexCompactedPayload struct {
	Message            string            `json:"message"`
	ReplacementHistory []json.RawMessage `json:"replacement_history"`
}

// Encode converts CIRDocument to target provider JSONL bytes.
func (c *CodexCodec) Encode(_ context.Context, doc domain.CIRDocument, _ domain.ProviderKind) ([]byte, error) {
	// Output format is this codec's provider (codex). full is CIR source if codex (reconstructed otherwise).
	full := doc.Envelope.SourceProvider == domain.ProviderCodex
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	// session_meta line — codex resume parser accepts empirical schema (codexSessionMetaOut comment reference).
	meta := codexSessionMetaOut{
		SessionID:     doc.Envelope.SessionOriginID,
		ID:            doc.Envelope.SessionOriginID,
		Timestamp:     doc.Envelope.CapturedAt,
		Cwd:           doc.Envelope.Cwd,
		Originator:    "cxt",
		CLIVersion:    "0.0.0-cxt",
		Source:        "cli",
		ThreadSource:  "user",
		ModelProvider: "openai",
	}
	metaPayload, _ := json.Marshal(meta)
	if err := enc.Encode(codexLine{Timestamp: doc.Envelope.CapturedAt, Type: "session_meta", Payload: metaPayload}); err != nil {
		return nil, fmt.Errorf("codex encode meta: %w", err)
	}

	// turn_context line — model is here instead of session_meta (same position as actual rollout;
	// non-standard "model" key in session_meta causes codex resume parser to reject the entire file).
	// decode's turn_context last-wins consumption pairs with roundtrip's source_model preservation.
	if doc.Envelope.SourceModel != "" {
		tcPayload, _ := json.Marshal(map[string]string{"model": doc.Envelope.SourceModel})
		if err := enc.Encode(codexLine{Timestamp: doc.Envelope.CapturedAt, Type: "turn_context", Payload: tcPayload}); err != nil {
			return nil, fmt.Errorf("codex encode turn_context: %w", err)
		}
	}

	events := normalizeCIR(doc).Events
	if full && needsCodexCompactedWrapper(events) {
		ts := doc.Envelope.CapturedAt
		if len(events) > 0 && events[0].Ts != "" {
			ts = events[0].Ts
		}
		events = []domain.Event{{
			Kind:                domain.EventCompaction,
			Ts:                  ts,
			Replacement:         events,
			ReplacementComplete: true,
		}}
	}

	for _, ev := range events {
		if ev.Kind == domain.EventCompaction && ev.Replacement != nil {
			if !ev.ReplacementComplete {
				// EffectiveContext deliberately falls back to the archival stream for
				// this case. Never emit a partial compacted marker that could make the
				// provider discard the complete replay we are about to send.
				continue
			}
			replacement, err := encodeCodexReplacement(ev.Replacement, full)
			if err != nil {
				return nil, err
			}
			payload, err := json.Marshal(codexCompactedPayload{ReplacementHistory: replacement})
			if err != nil {
				return nil, fmt.Errorf("codex encode compacted: %w", err)
			}
			if err := enc.Encode(codexLine{Timestamp: ev.Ts, Type: "compacted", Payload: payload}); err != nil {
				return nil, fmt.Errorf("codex encode compacted: %w", err)
			}
			// A compacted replacement is model-visible but response items nested
			// inside it do not populate the Codex TUI transcript. Re-emit only the
			// ordinary visible messages as event_msg companions. Provider-internal
			// agent_message items deliberately remain hidden, matching native
			// rollouts (their nearby display events are not text-equivalent).
			for _, replacementEvent := range normalizeEvents(ev.Replacement) {
				item, ok := codexEventToItem(replacementEvent, full, true)
				if !ok {
					return nil, fmt.Errorf("codex encode replacement display: unsupported %s event", replacementEvent.Kind)
				}
				if err := emitCodexDisplayCompanion(enc, replacementEvent.Ts, item); err != nil {
					return nil, err
				}
			}
			continue
		}
		it, ok := codexEventToItem(ev, full, false)
		if !ok {
			continue
		}
		payload, err := json.Marshal(it)
		if err != nil {
			return nil, fmt.Errorf("codex encode item: %w", err)
		}
		if err := enc.Encode(codexLine{Timestamp: ev.Ts, Type: "response_item", Payload: payload}); err != nil {
			return nil, fmt.Errorf("codex encode: %w", err)
		}
		if err := emitCodexDisplayCompanion(enc, ev.Ts, it); err != nil {
			return nil, err
		}
	}
	// Token statistics reattachment: aggregate values (envelope) into a single token_count event_msg line
	// to make decode→encode→decode round-trip lossless (like Claude's last usage reattachment).
	if doc.Envelope.ContextTokens > 0 || doc.Envelope.OutputTokens > 0 {
		info := codexTokenInfo{
			TotalTokenUsage: &codexTokenUsage{OutputTokens: doc.Envelope.OutputTokens},
			LastTokenUsage:  &codexTokenUsage{InputTokens: doc.Envelope.ContextTokens},
		}
		payload, _ := json.Marshal(codexEventMsg{Type: "token_count", Info: &info})
		ts := doc.Envelope.CapturedAt
		if len(events) > 0 && events[len(events)-1].Ts != "" {
			ts = events[len(events)-1].Ts
		}
		if err := enc.Encode(codexLine{Timestamp: ts, Type: "event_msg", Payload: payload}); err != nil {
			return nil, fmt.Errorf("codex encode token_count: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// emitCodexDisplayCompanion mirrors the auxiliary records written by Codex for
// ordinary conversation messages. Model replay reads response items (including
// compacted.replacement_history), while the TUI reconstructs visible transcript
// rows from event_msg. Decode intentionally ignores these companions, so they do
// not duplicate canonical CIR events on the next capture.
func emitCodexDisplayCompanion(enc *json.Encoder, ts string, item codexResponseItem) error {
	if item.Type != "message" || len(item.Content) == 0 {
		return nil
	}
	if item.Role != "user" && item.Role != "assistant" {
		return nil
	}
	text := ""
	for _, content := range item.Content {
		text += content.Text
	}
	if text == "" {
		return nil
	}

	var payload []byte
	if item.Role == "user" {
		payload, _ = json.Marshal(map[string]interface{}{
			"type": "user_message", "message": text,
			"images": []string{}, "local_images": []string{}, "text_elements": []string{},
		})
	} else {
		payload, _ = json.Marshal(map[string]interface{}{"type": "agent_message", "message": text})
	}
	if err := enc.Encode(codexLine{Timestamp: ts, Type: "event_msg", Payload: payload}); err != nil {
		return fmt.Errorf("codex encode event_msg: %w", err)
	}
	return nil
}

func needsCodexCompactedWrapper(events []domain.Event) bool {
	hasLockedState := false
	for _, ev := range events {
		if ev.Kind != domain.EventCompaction {
			continue
		}
		if ev.Replacement != nil {
			return false
		}
		if ev.Locked != nil && ev.Locked.Provider == domain.ProviderCodex && ev.Locked.Scheme == "encrypted_content" {
			hasLockedState = true
		}
	}
	return hasLockedState
}

func codexEventToItem(ev domain.Event, full, withinReplacement bool) (codexResponseItem, bool) {
	var it codexResponseItem
	switch ev.Kind {
	case domain.EventMessage:
		if ev.AgentMessage && full {
			it.Type = "agent_message"
			it.Author = ev.AgentAuthor
			it.Recipient = ev.AgentRecipient
			for _, block := range ev.Blocks {
				it.Content = append(it.Content, codexContent{Type: "input_text", Text: block.Text})
			}
			if ev.Locked != nil && ev.Locked.Provider == domain.ProviderCodex && ev.Locked.Scheme == "encrypted_content" {
				it.Content = append(it.Content, codexContent{Type: "encrypted_content", EncryptedContent: ev.Locked.Blob})
			}
		} else {
			it.Type = "message"
			it.Role = ev.Role
			contentType := "output_text"
			if ev.Role == "user" || ev.Role == "developer" || ev.Role == "system" {
				contentType = "input_text"
			}
			for _, block := range ev.Blocks {
				it.Content = append(it.Content, codexContent{Type: contentType, Text: block.Text})
			}
		}
	case domain.EventToolCall:
		name := ev.ProviderToolName
		if !full || name == "" {
			name = providerToolName(domain.ProviderCodex, ev.ToolName)
		}
		it.Type = "function_call"
		it.Name = name
		it.CallID = ev.CallID
		it.Arguments, _ = json.Marshal(marshalToolInput(ev.Input))
		it.Status = ev.Status
	case domain.EventToolResult:
		it.Type = "function_call_output"
		it.CallID = ev.CallID
		out, _ := json.Marshal(codexToolResultOutput(ev.Output, full))
		it.Output = out
	case domain.EventReasoning:
		if full && ev.Locked != nil && ev.Locked.Scheme == "encrypted_content" {
			it.Type = "reasoning"
			it.EncryptedContent = ev.Locked.Blob
			it.Summary = []interface{}{}
			if ev.RedactedSummary != "" {
				it.Summary = []interface{}{map[string]interface{}{"type": "summary_text", "text": ev.RedactedSummary}}
			}
		} else {
			// Reconstructed: locked state stays inert. Plain summary is replayed
			// as assistant text when one exists.
			if ev.RedactedSummary == "" {
				return codexResponseItem{}, false
			}
			it.Type = "message"
			it.Role = "assistant"
			it.Content = []codexContent{{Type: "output_text", Text: ev.RedactedSummary}}
		}
	case domain.EventCompaction:
		if !withinReplacement || ev.Replacement != nil || !full || ev.Locked == nil || ev.Locked.Provider != domain.ProviderCodex || ev.Locked.Scheme != "encrypted_content" {
			return codexResponseItem{}, false
		}
		it.Type = "compaction"
		it.EncryptedContent = ev.Locked.Blob
	default:
		return codexResponseItem{}, false
	}
	if full {
		it.ID = ev.ID
		if ev.ProviderMetadata != nil {
			metadata := *ev.ProviderMetadata
			it.ProviderMetadata = &metadata
		}
	}
	return it, true
}

func encodeCodexReplacement(events []domain.Event, full bool) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(events))
	for _, ev := range normalizeEvents(events) {
		item, ok := codexEventToItem(ev, full, true)
		if !ok {
			return nil, fmt.Errorf("codex encode replacement: unsupported %s event", ev.Kind)
		}
		data, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("codex encode replacement item: %w", err)
		}
		out = append(out, data)
	}
	return out, nil
}

// Ensure CodexCodec implements outbound.ProviderCodec.
var _ outbound.ProviderCodec = (*CodexCodec)(nil)
