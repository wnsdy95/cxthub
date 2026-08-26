package codec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
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

// Provider returns the provider (codex) this codec is responsible for.
func (c *CodexCodec) Provider() domain.ProviderKind { return domain.ProviderCodex }

// codexLine is a single line from a rollout JSONL: {timestamp, type, payload}.
type codexLine struct {
	Timestamp string          `json:"timestamp,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp,omitempty"`
	Cwd           string `json:"cwd"`
	CLIVersion    string `json:"cli_version,omitempty"`
	ModelProvider string `json:"model_provider,omitempty"`
	Model         string `json:"model,omitempty"`
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

// codexTurnContext is the portion of the turn_context payload we use (per-turn model).
type codexTurnContext struct {
	Model string `json:"model,omitempty"`
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

type codexResponseItemHeader struct {
	Type string `json:"type"`
}

type codexCompactedPayload struct {
	Message            string            `json:"message"`
	ReplacementHistory []json.RawMessage `json:"replacement_history"`
}

func decodeCodexCompactedPayload(raw json.RawMessage) (codexCompactedPayload, bool, error) {
	var payload codexCompactedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return codexCompactedPayload{}, false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return codexCompactedPayload{}, false, err
	}
	history, present := fields["replacement_history"]
	if !present || bytes.Equal(bytes.TrimSpace(history), []byte("null")) {
		return payload, false, nil
	}
	return payload, true, nil
}

// Decode converts Codex rollout JSONL bytes to CIRDocument.
func (c *CodexCodec) Decode(_ context.Context, raw []byte) (domain.CIRDocument, error) {
	doc := domain.CIRDocument{}
	doc.Envelope.CIRVersion = domain.CIRVersionV1
	doc.Envelope.SourceProvider = domain.ProviderCodex
	doc.Envelope.Fidelity = domain.FidelityFull

	var events []domain.Event
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ln codexLine
		if err := json.Unmarshal(line, &ln); err != nil {
			return domain.CIRDocument{}, fmt.Errorf("codex decode: %w", err)
		}
		switch ln.Type {
		case "session_meta":
			var meta codexSessionMeta
			if err := json.Unmarshal(ln.Payload, &meta); err != nil {
				return domain.CIRDocument{}, fmt.Errorf("codex decode session_meta: %w", err)
			}
			setIfEmpty(&doc.Envelope.SessionOriginID, meta.ID)
			setIfEmpty(&doc.Envelope.Cwd, meta.Cwd)
			noteModel(&doc.Envelope, meta.Model)
			setIfEmpty(&doc.Envelope.CapturedAt, firstNonEmpty(meta.Timestamp, ln.Timestamp))
		case "turn_context":
			// Per-turn model (session switch /model) → same as Claude: last-wins + cumulative semantics.
			var tc codexTurnContext
			if err := json.Unmarshal(ln.Payload, &tc); err == nil {
				noteModel(&doc.Envelope, tc.Model)
			}
		case "event_msg":
			// Consume only token_count (token statistics). Drop other event_msg (auxiliary UI metadata).
			var em codexEventMsg
			if err := json.Unmarshal(ln.Payload, &em); err == nil && em.Type == "token_count" && em.Info != nil {
				if u := em.Info.TotalTokenUsage; u != nil && u.OutputTokens > 0 {
					doc.Envelope.OutputTokens = u.OutputTokens // session cumulative value → last-wins
				}
				// Context window size = input from previous request (including cache) — size at end of last observation.
				if u := em.Info.LastTokenUsage; u != nil && u.InputTokens > 0 {
					doc.Envelope.ContextTokens = u.InputTokens
				}
			}
		case "response_item":
			// Codex also emits control-plane response items such as tool_search_call,
			// tool_search_output, and image_generation_call. Their payloads use schemas
			// that intentionally differ from conversation tool calls (for example,
			// tool_search_call.arguments is an object and tool_search_output contains
			// repeated tool schemas). Discriminate first so auxiliary records cannot
			// break decoding or bloat persisted CIR documents.
			var header codexResponseItemHeader
			if err := json.Unmarshal(ln.Payload, &header); err != nil {
				return domain.CIRDocument{}, fmt.Errorf("codex decode response_item header: %w", err)
			}
			if !isCodexConversationItem(header.Type) {
				continue
			}
			var it codexResponseItem
			if err := json.Unmarshal(ln.Payload, &it); err != nil {
				return domain.CIRDocument{}, fmt.Errorf("codex decode response_item: %w", err)
			}
			ev, ok := codexItemToEvent(it, ln.Timestamp)
			if ok {
				events = append(events, ev)
			}
		case "compacted":
			// Keep both layers: the top-level event stream remains the complete
			// archival transcript, while replacement_history is nested on an
			// explicit boundary and becomes the provider-visible replay suffix.
			// Current Codex rollouts leave message empty and carry an encrypted
			// compaction response item inside replacement_history, so dropping
			// the latter would make archival history look like active context.
			doc.Envelope.CompactionCount++
			cp, historyPresent, err := decodeCodexCompactedPayload(ln.Payload)
			if err != nil {
				return domain.CIRDocument{}, fmt.Errorf("codex decode compacted: %w", err)
			}
			replacement := []domain.Event{}
			complete := false
			if historyPresent {
				replacement, complete, err = decodeCodexReplacement(cp.ReplacementHistory, ln.Timestamp)
				if err != nil {
					return domain.CIRDocument{}, err
				}
			}
			events = append(events, domain.Event{
				Kind: domain.EventCompaction, Ts: ln.Timestamp,
				Replacement:         replacement,
				ReplacementComplete: complete,
			})
			if cp.Message != "" {
				events = append(events, domain.Event{
					Kind: domain.EventMessage, Role: "user", Ts: ln.Timestamp,
					Blocks:         []domain.ContentBlock{{Type: "text", Text: cp.Message}},
					CompactSummary: true,
				})
			}
		default:
			// Auxiliary metadata like item → drop
		}
	}
	if err := sc.Err(); err != nil {
		return domain.CIRDocument{}, fmt.Errorf("codex decode scan: %w", err)
	}
	doc.Events = assignSeq(events)
	doc.Envelope.CIRVersion = domain.CIRVersionForEvents(doc.Events)
	return doc, nil
}

func isCodexConversationItem(itemType string) bool {
	switch itemType {
	case "message",
		"agent_message",
		"function_call",
		"custom_tool_call",
		"function_call_output",
		"custom_tool_call_output",
		"web_search_call",
		"reasoning",
		"compaction":
		return true
	default:
		return false
	}
}

func codexItemToEvent(it codexResponseItem, ts string) (domain.Event, bool) {
	ev := domain.Event{ID: it.ID, Ts: ts}
	if it.ProviderMetadata != nil {
		metadata := *it.ProviderMetadata
		ev.ProviderMetadata = &metadata
	}
	switch it.Type {
	case "message":
		ev.Kind = domain.EventMessage
		ev.Role = it.Role
		if ev.Role == "" {
			ev.Role = "assistant"
		}
		blocks := make([]domain.ContentBlock, 0, len(it.Content))
		for _, ct := range it.Content {
			blocks = append(blocks, domain.ContentBlock{Type: "text", Text: ct.Text})
		}
		if len(blocks) == 0 {
			blocks = []domain.ContentBlock{{Type: "text", Text: ""}}
		}
		ev.Blocks = blocks
		return ev, true
	case "agent_message":
		ev.Kind = domain.EventMessage
		ev.Role = "assistant"
		ev.AgentMessage = true
		ev.AgentAuthor = it.Author
		ev.AgentRecipient = it.Recipient
		for _, content := range it.Content {
			switch content.Type {
			case "input_text", "output_text":
				ev.Blocks = append(ev.Blocks, domain.ContentBlock{Type: "text", Text: content.Text})
			case "encrypted_content":
				if content.EncryptedContent != "" {
					ev.Locked = &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: content.EncryptedContent}
				}
			}
		}
		if ev.Blocks == nil {
			ev.Blocks = []domain.ContentBlock{}
		}
		if len(ev.Blocks) == 0 && ev.Locked == nil {
			return domain.Event{}, false
		}
		return ev, true
	case "function_call":
		ev.Kind = domain.EventToolCall
		ev.CallID = it.CallID
		ev.ProviderToolName = it.Name
		ev.ToolName = canonicalToolName(it.Name)
		ev.Status = it.Status
		ev.Input = parseCodexFunctionArguments(it.Arguments)
		return ev, true
	case "custom_tool_call":
		ev.Kind = domain.EventToolCall
		ev.CallID = it.CallID
		ev.ProviderToolName = it.Name
		ev.ToolName = canonicalToolName(it.Name)
		ev.Status = it.Status
		ev.Input = parseToolInput(it.Input)
		return ev, true
	case "function_call_output", "custom_tool_call_output":
		ev.Kind = domain.EventToolResult
		ev.CallID = it.CallID
		ev.Output = normalizeToolResultOutput(decodeJSONValue(it.Output))
		return ev, true
	case "web_search_call":
		ev.Kind = domain.EventToolCall
		ev.CallID = it.CallID
		ev.ProviderToolName = "web_search"
		ev.ToolName = toolWebSearch
		ev.Status = it.Status
		q := ""
		if it.Action != nil {
			q = it.Action.Query
		}
		ev.Input = map[string]interface{}{"query": q}
		return ev, true
	case "reasoning":
		ev.Kind = domain.EventReasoning
		ev.Locked = &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: it.EncryptedContent}
		ev.CrossReplayable = false
		// Combine structured array summary into plain text to preserve — cross-codec (codex→claude) where
		// the summary survives the same as Claude thinking. Encode(full) re-emits as a single
		// summary_text, so CIR round-trip is stable (original multi-items flattened by \n\n).
		ev.RedactedSummary = summaryToString(it.Summary)
		return ev, true
	case "compaction":
		if it.EncryptedContent == "" {
			return domain.Event{}, false
		}
		ev.Kind = domain.EventCompaction
		ev.Locked = &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: it.EncryptedContent}
		ev.CrossReplayable = false
		return ev, true
	default:
		return domain.Event{}, false
	}
}

func decodeCodexReplacement(rawItems []json.RawMessage, ts string) ([]domain.Event, bool, error) {
	replacement := make([]domain.Event, 0, len(rawItems))
	complete := true
	for _, raw := range rawItems {
		var header codexResponseItemHeader
		if err := json.Unmarshal(raw, &header); err != nil {
			// The outer compacted record is still usable as an archival boundary.
			// Do not fail capture or claim that a partially decoded replacement is
			// authoritative when a future provider item cannot be classified.
			complete = false
			continue
		}
		if !isCodexConversationItem(header.Type) {
			complete = false
			continue
		}
		if !codexReplacementRawItemFullyUnderstood(raw, header.Type) {
			complete = false
			continue
		}
		var item codexResponseItem
		if err := json.Unmarshal(raw, &item); err != nil {
			complete = false
			continue
		}
		if !codexReplacementItemFullyUnderstood(item) {
			complete = false
			continue
		}
		if ev, ok := codexItemToEvent(item, ts); ok {
			replacement = append(replacement, ev)
		} else {
			complete = false
		}
	}
	return assignSeq(replacement), complete, nil
}

// codexReplacementRawItemFullyUnderstood closes json.Unmarshal's permissive
// unknown-field gap. Known replay identity fields are modeled explicitly; any
// new top-level, content, or metadata field makes the boundary non-authoritative
// until the codec learns how to preserve it.
func codexReplacementRawItemFullyUnderstood(raw json.RawMessage, itemType string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	allowed := map[string]bool{
		"type": true, "id": true, "internal_chat_message_metadata_passthrough": true,
	}
	switch itemType {
	case "message":
		allowed["role"], allowed["content"] = true, true
	case "agent_message":
		allowed["author"], allowed["recipient"], allowed["content"] = true, true, true
	case "reasoning":
		allowed["encrypted_content"], allowed["summary"] = true, true
	case "function_call":
		allowed["name"], allowed["arguments"], allowed["call_id"], allowed["status"] = true, true, true, true
	case "function_call_output":
		allowed["call_id"], allowed["output"] = true, true
	case "custom_tool_call":
		allowed["name"], allowed["input"], allowed["call_id"], allowed["status"] = true, true, true, true
	case "custom_tool_call_output":
		allowed["call_id"], allowed["output"] = true, true
	case "web_search_call":
		allowed["action"], allowed["call_id"], allowed["status"] = true, true, true
	case "compaction":
		allowed["encrypted_content"] = true
	default:
		return false
	}
	for key := range fields {
		if !allowed[key] {
			return false
		}
	}
	if metadataRaw, ok := fields["internal_chat_message_metadata_passthrough"]; ok {
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(metadataRaw, &metadata); err != nil || metadata == nil {
			return false
		}
		for key := range metadata {
			if key != "turn_id" && key != "create_time" {
				return false
			}
		}
	}
	if contentRaw, ok := fields["content"]; ok && !codexReplacementContentFullyUnderstood(contentRaw) {
		return false
	}
	if summaryRaw, ok := fields["summary"]; ok && !codexReplacementSummaryWireFullyUnderstood(summaryRaw) {
		return false
	}
	return true
}

func codexReplacementContentFullyUnderstood(raw json.RawMessage) bool {
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		return false
	}
	for _, entry := range content {
		var kind string
		if err := json.Unmarshal(entry["type"], &kind); err != nil {
			return false
		}
		for key := range entry {
			switch kind {
			case "input_text", "output_text":
				if key != "type" && key != "text" {
					return false
				}
			case "encrypted_content":
				if key != "type" && key != "encrypted_content" {
					return false
				}
			default:
				return false
			}
		}
		switch kind {
		case "input_text", "output_text":
			if _, ok := entry["text"]; !ok {
				return false
			}
		case "encrypted_content":
			if _, ok := entry["encrypted_content"]; !ok {
				return false
			}
		}
	}
	return true
}

func codexReplacementSummaryWireFullyUnderstood(raw json.RawMessage) bool {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false
	}
	for _, entry := range entries {
		var text string
		if json.Unmarshal(entry, &text) == nil {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil {
			return false
		}
		for key := range fields {
			if key != "type" && key != "text" {
				return false
			}
		}
	}
	return true
}

// codexReplacementItemFullyUnderstood is deliberately stricter than normal
// archival decoding. A replacement is an authoritative projection only when
// every provider-visible item can be represented without semantic loss. New
// item or content variants therefore fall back to replaying the full archive.
func codexReplacementItemFullyUnderstood(item codexResponseItem) bool {
	switch item.Type {
	case "message":
		switch item.Role {
		case "user", "assistant", "system", "developer":
		default:
			return false
		}
		for _, content := range item.Content {
			if content.Type != "input_text" && content.Type != "output_text" {
				return false
			}
		}
		return true
	case "agent_message":
		encrypted := 0
		for _, content := range item.Content {
			switch content.Type {
			case "input_text", "output_text":
			case "encrypted_content":
				encrypted++
				if encrypted > 1 || content.EncryptedContent == "" {
					return false
				}
			default:
				return false
			}
		}
		return true
	case "reasoning":
		return codexSummaryFullyUnderstood(item.Summary)
	case "function_call", "function_call_output":
		return true
	case "custom_tool_call", "custom_tool_call_output":
		// CIR's legacy tool shape does not retain whether Codex used the custom
		// item wire variant. Keep the archive, but do not claim that a replacement
		// containing it is an authoritative lossless projection.
		return false
	case "web_search_call":
		// CIR's normalized web-search event keeps the query but intentionally
		// omits provider navigation state (open_page/find_in_page URL, pattern,
		// multi-query arrays). It is useful in the archive but not lossless
		// enough to make a replacement authoritative.
		return false
	case "compaction":
		return item.EncryptedContent != ""
	default:
		return false
	}
}

func codexSummaryFullyUnderstood(items []interface{}) bool {
	for _, item := range items {
		switch value := item.(type) {
		case string:
			continue
		case map[string]interface{}:
			kind, _ := value["type"].(string)
			if kind != "summary_text" {
				return false
			}
			if _, ok := value["text"].(string); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// parseCodexFunctionArguments accepts the conventional JSON-encoded string and
// the object form observed in newer response item variants. Non-object values
// remain lossless through CIR's _raw escape hatch.
func parseCodexFunctionArguments(raw json.RawMessage) map[string]interface{} {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]interface{}{}
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return parseToolInput(text)
	}

	var object map[string]interface{}
	if err := json.Unmarshal(trimmed, &object); err == nil && object != nil {
		return object
	}
	return map[string]interface{}{"_raw": string(trimmed)}
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

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// summaryToString combines codex reasoning.summary (structured array) into plain text.
// Items can be plain string or {type:"summary_text", text:...} objects on either side.
func summaryToString(items []interface{}) string {
	var parts []string
	for _, it := range items {
		switch v := it.(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		case map[string]interface{}:
			if s, _ := v["text"].(string); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// Ensure CodexCodec implements outbound.ProviderCodec.
var _ outbound.ProviderCodec = (*CodexCodec)(nil)
