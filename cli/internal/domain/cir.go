package domain

import (
	"encoding/json"
	"fmt"
)

// CIRDocument is an independent normalized dialogue representation (CIR v1/v2, domain model).
// Decodes Claude JSONL and Codex rollout JSONL into this format,
// and encodes from this format to the target provider format.
//
// Normal schema: schemas/cir.schema.json (draft 2020-12).
type CIRDocument struct {
	// Envelope contains session global metadata.
	Envelope Envelope `json:"envelope"`
	// Events is a time-ordered (seq ascending) event stream.
	Events []Event `json:"events"`
}

// EffectiveContext returns the provider-visible context after the latest
// compaction boundary while leaving the archival Events stream untouched.
// Legacy documents without an explicit boundary replay their complete stream.
// A boundary's non-nil Replacement distinguishes a real boundary from the
// provider-locked compaction state item that can appear inside that replacement.
func (d CIRDocument) EffectiveContext() CIRDocument {
	latest, complete := d.latestCompactionBoundary()
	if latest < 0 || !complete {
		return d
	}
	out := d
	out.Events = make([]Event, 0, len(d.Events[latest].Replacement)+len(d.Events)-latest-1)
	out.Events = append(out.Events, d.Events[latest].Replacement...)
	out.Events = append(out.Events, d.Events[latest+1:]...)
	// Replacement items have their own local sequence space while archival
	// suffix events retain transcript-global seq values. Renumber the joined
	// replay stream so codec sorting cannot move a later user turn in front of
	// the encrypted compaction state it depends on.
	for i := range out.Events {
		out.Events[i].Seq = i
	}
	return out
}

// HasCompleteCompactionBoundary reports whether the latest archival boundary
// has an authoritative provider replacement. Consumers that reconstruct the
// provider's native wire format use this to preserve the replacement operation
// after applying replay budgets.
func (d CIRDocument) HasCompleteCompactionBoundary() bool {
	_, complete := d.LatestCompactionReplacementCount()
	return complete
}

// LatestCompactionReplacementCount returns the size of the authoritative
// replacement prefix in EffectiveContext. A zero-length complete replacement
// is distinct from no/incomplete boundary through the boolean result.
func (d CIRDocument) LatestCompactionReplacementCount() (int, bool) {
	latest, complete := d.latestCompactionBoundary()
	if latest < 0 || !complete {
		return 0, false
	}
	return len(d.Events[latest].Replacement), true
}

func (d CIRDocument) latestCompactionBoundary() (int, bool) {
	latest := -1
	for i := range d.Events {
		if d.Events[i].Kind == EventCompaction && d.Events[i].Replacement != nil {
			latest = i
		}
	}
	if latest < 0 {
		return -1, false
	}
	return latest, d.Events[latest].ReplacementComplete
}

// Envelope contains session global metadata (domain model).
type Envelope struct {
	// CIRVersion is the CIR schema version ("1" or "2").
	CIRVersion string `json:"cir_version"`
	// SourceProvider is the original capture provider (claude|codex).
	SourceProvider ProviderKind `json:"source_provider"`
	// SourceModel is the original model identifier (e.g., "claude-opus-4-8").
	SourceModel string `json:"source_model"`
	// CapturedAt is the capture timestamp (RFC3339 string).
	CapturedAt string `json:"captured_at"`
	// Cwd is the absolute working directory at the capture point.
	Cwd string `json:"cwd"`
	// GitBranch is the git branch name at the capture point.
	// Claude is embedded in the record, Codex is supplemented by the gitctx adapter.
	GitBranch string `json:"git_branch"`
	// SessionOriginID is the original session identifier.
	// Claude is sessionId, Codex is rollout uuid.
	SessionOriginID string `json:"session_origin_id"`
	// Fidelity is the fidelity tier of this entire document.
	Fidelity FidelityTier `json:"fidelity"`
	// ContextTokens is the context window size at the session end point (sum of the input, cache_creation, and cache_read of the last assistant message). 0 means unobserved (legacy capture) — omitempty has no effect on canonical bytes/hash.
	ContextTokens int `json:"context_tokens,omitempty"`
	// OutputTokens is the total number of assistant output tokens in the session. 0 means unobserved.
	OutputTokens int `json:"output_tokens,omitempty"`
	// SourceModels is all models that appeared in the session (in order of appearance, no duplicates). SourceModel is the last used model (representative value). During re-encoding, message-specific models are flattened to SourceModel, so they are complete only in the original doc (display-only metadata — omitempty, legacy doc hash unchanged).
	SourceModels []string `json:"source_models,omitempty"`
	// CompactionCount is the number of context compressions (compact_boundary) observed in this session transcript. Claude counts type=="system"&&subtype=="compact_boundary" markers. Compression does not change the sessionId, so a separate signal is needed to mark session boundaries — the graph draws a compression marker (◈) in snapshots with a larger count than the parent. 0 means omitempty has no effect on canonical bytes/hash (legacy capture with no compression results in byte equality → dedup unchanged).
	CompactionCount int `json:"compaction_count,omitempty"`
}

// OrderedModels is a list of models (list/graph display metadata) —
// the representative (last used, SourceModel) model is first, others in reverse order of recent appearance.
// Data source for the "leftmost (top) = this commit's direct tool" rule of web overlays (AIDots).
func (e Envelope) OrderedModels() []string {
	if len(e.SourceModels) == 0 {
		if e.SourceModel == "" {
			return nil
		}
		return []string{e.SourceModel}
	}
	var out []string
	if e.SourceModel != "" {
		out = append(out, e.SourceModel)
	}
	for i := len(e.SourceModels) - 1; i >= 0; i-- {
		if m := e.SourceModels[i]; m != e.SourceModel {
			out = append(out, m)
		}
	}
	return out
}

// Event is a single element of the CIR event stream (domain model).
// It represents a union type with a common field and a payload specific to each Kind.
//
// Allowed Kind values: "turn", "message", "tool_call", "tool_result", "reasoning", "compaction".
//
// JSON serialization ensures that MarshalJSON/UnmarshalJSON only emit/receive fields specific to each Kind, satisfying the oneOf union in schema(cir.schema.json).
type Event struct {
	// Kind is an event type tag.
	Kind string
	// ID is the original identifier or creation ID.
	ID string
	// Ts is an RFC3339 timestamp (optional).
	Ts string
	// Seq is the regular sort order and sort key.
	Seq int
	// ProviderMetadata preserves explicitly modeled provider-local replay
	// metadata. It is reinserted only for same-provider full-fidelity replay.
	ProviderMetadata *ProviderMetadata
	// fieldPresence records optional JSON fields whose zero values would
	// otherwise disappear during unmarshal. It is intentionally not serialized;
	// MarshalJSON uses it to preserve canonical zero values and version/union
	// validation uses it to reject fields that are present under the wrong CIR.
	fieldPresence eventFieldPresence

	// --- kind = "turn" payload ---
	// Role is the role of the turn/message (user|assistant|system|developer).
	Role string

	// --- kind = "message" payload ---
	// Blocks is a list of message content blocks.
	Blocks []ContentBlock
	// CompactSummary indicates that this message is a summary generated by the agent's context compression (claude: isCompactSummary record). It serves as the primary Summary source for memorized data written by the agent. omitempty — preserves legacy doc hash.
	CompactSummary bool
	// AgentMessage preserves Codex multi-agent messages that are part of the
	// provider active context. Visible text remains in Blocks; provider-locked
	// state stays in Locked and is reinserted only for same-provider replay.
	AgentMessage   bool
	AgentAuthor    string
	AgentRecipient string

	// --- kind = "compaction" payload ---
	// Replacement is the provider's effective context at a compaction boundary.
	// A non-nil empty slice is meaningful (Claude boundary observed before its
	// summary record). The complete pre-compaction transcript remains in the
	// top-level Events stream for audit and lossless storage.
	Replacement []Event
	// ReplacementComplete proves that every provider replacement item was
	// understood. Unknown future item types fail safe to archival replay rather
	// than silently projecting a partial active context.
	ReplacementComplete bool

	// --- kind = "tool_call" payload ---
	// CallID is the tool call identifier (matches tool_result).
	CallID string
	// ToolName is a standardized tool name (provider-independent vocabulary, domain model).
	ToolName string
	// ProviderToolName is the original tool name (round-trip preservation).
	ProviderToolName string
	// Input is the tool call input object (JSON deserialized map).
	Input map[string]interface{}
	// Status is the tool call status (optional).
	Status string

	// --- kind = "tool_result" payload ---
	// Output is the tool result (string, object, or provider content block array).
	Output interface{}
	// IsError indicates whether the tool execution was an error (optional).
	IsError bool

	// --- kind = "reasoning" payload ---
	// Locked is the provider-locked original (claude: signature, codex: encrypted_content).
	// nil means no locked reasoning.
	Locked *LockedBlob
	// RedactedSummary is the plain text summary (cross-provider replay/memory use).
	RedactedSummary string
	// CrossReplayable is the ability to cross-replay. Locked reasoning is always false.
	CrossReplayable bool
}

// Event kind constants.
const (
	EventTurn       = "turn"
	EventMessage    = "message"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventReasoning  = "reasoning"
	EventCompaction = "compaction"
)

// MarshalJSON serializes an Event to a schema (cir.schema.json) kind-specific union form.
// Only common fields (kind/seq always, id/ts non-empty if present) and kind-specific fields are emitted.
func (e Event) MarshalJSON() ([]byte, error) {
	if err := e.validateKindFieldPresence(); err != nil {
		return nil, err
	}
	m := map[string]interface{}{
		"kind": e.Kind,
		"seq":  e.Seq,
	}
	if e.ID != "" {
		m["id"] = e.ID
	}
	if e.Ts != "" {
		m["ts"] = e.Ts
	}
	if e.ProviderMetadata != nil {
		m["provider_metadata"] = e.ProviderMetadata
	}
	switch e.Kind {
	case EventTurn:
		m["role"] = e.Role
	case EventMessage:
		m["role"] = e.Role
		blocks := e.Blocks
		if blocks == nil {
			blocks = []ContentBlock{}
		}
		m["blocks"] = blocks
		if e.CompactSummary {
			m["compact_summary"] = true
		}
		if e.AgentMessage || e.fieldPresence&eventFieldAgentMessage != 0 {
			m["agent_message"] = e.AgentMessage
			if e.AgentAuthor != "" || e.fieldPresence&eventFieldAgentAuthor != 0 {
				m["agent_author"] = e.AgentAuthor
			}
			if e.AgentRecipient != "" || e.fieldPresence&eventFieldAgentRecipient != 0 {
				m["agent_recipient"] = e.AgentRecipient
			}
			if e.Locked != nil {
				m["locked"] = e.Locked
			}
		}
	case EventToolCall:
		m["call_id"] = e.CallID
		m["tool_name"] = e.ToolName
		if e.ProviderToolName != "" {
			m["provider_tool_name"] = e.ProviderToolName
		}
		input := e.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		m["input"] = input
		if e.Status != "" {
			m["status"] = e.Status
		}
	case EventToolResult:
		m["call_id"] = e.CallID
		// schema: output is required and type [string,object,array] — nil is forced to "".
		if e.Output == nil {
			m["output"] = ""
		} else {
			m["output"] = e.Output
		}
		if e.IsError {
			m["is_error"] = true
		}
	case EventReasoning:
		if e.Locked != nil {
			m["locked"] = e.Locked
		}
		if e.RedactedSummary != "" {
			m["redacted_summary"] = e.RedactedSummary
		}
		// schema: cross_replayable is const false — locked reasoning is never cross-replayed (provider compatibility rules).
		m["cross_replayable"] = false
	case EventCompaction:
		if e.Replacement != nil && e.Locked != nil {
			return nil, fmt.Errorf("cir: compaction event cannot be both a replacement boundary and locked state")
		}
		if e.Replacement != nil {
			m["replacement"] = e.Replacement
			m["replacement_complete"] = e.ReplacementComplete
		}
		if e.Locked != nil {
			m["locked"] = e.Locked
		}
		if e.Replacement == nil && e.Locked == nil {
			return nil, fmt.Errorf("cir: compaction event needs replacement boundary or locked state")
		}
	default:
		return nil, fmt.Errorf("cir: unknown event kind %q", e.Kind)
	}
	return json.Marshal(m)
}

// eventWire is the flattened JSON representation for deserializing an Event.
type eventWire struct {
	Kind                string                 `json:"kind"`
	ID                  string                 `json:"id"`
	Ts                  string                 `json:"ts"`
	Seq                 int                    `json:"seq"`
	Role                string                 `json:"role"`
	Blocks              []ContentBlock         `json:"blocks"`
	CallID              string                 `json:"call_id"`
	ToolName            string                 `json:"tool_name"`
	ProviderToolName    string                 `json:"provider_tool_name"`
	Input               map[string]interface{} `json:"input"`
	Status              string                 `json:"status"`
	Output              interface{}            `json:"output"`
	IsError             bool                   `json:"is_error"`
	Locked              *LockedBlob            `json:"locked"`
	RedactedSummary     string                 `json:"redacted_summary"`
	CrossReplayable     bool                   `json:"cross_replayable"`
	CompactSummary      bool                   `json:"compact_summary"`
	AgentMessage        bool                   `json:"agent_message"`
	AgentAuthor         string                 `json:"agent_author"`
	AgentRecipient      string                 `json:"agent_recipient"`
	Replacement         []Event                `json:"replacement"`
	ReplacementComplete bool                   `json:"replacement_complete"`
	ProviderMetadata    *ProviderMetadata      `json:"provider_metadata"`
}

type eventFieldPresence uint16

const (
	eventFieldProviderMetadata eventFieldPresence = 1 << iota
	eventFieldAgentMessage
	eventFieldAgentAuthor
	eventFieldAgentRecipient
	eventFieldReplacement
	eventFieldReplacementComplete
	eventFieldLocked
)

const eventV2PresenceMask = eventFieldProviderMetadata |
	eventFieldAgentMessage |
	eventFieldAgentAuthor |
	eventFieldAgentRecipient |
	eventFieldReplacement |
	eventFieldReplacementComplete

func eventPresence(fields map[string]json.RawMessage) eventFieldPresence {
	var present eventFieldPresence
	for name, bit := range map[string]eventFieldPresence{
		"provider_metadata":    eventFieldProviderMetadata,
		"agent_message":        eventFieldAgentMessage,
		"agent_author":         eventFieldAgentAuthor,
		"agent_recipient":      eventFieldAgentRecipient,
		"replacement":          eventFieldReplacement,
		"replacement_complete": eventFieldReplacementComplete,
		"locked":               eventFieldLocked,
	} {
		if _, ok := fields[name]; ok {
			present |= bit
		}
	}
	return present
}

func (e Event) validateKindFieldPresence() error {
	if e.ProviderMetadata == nil && e.fieldPresence&eventFieldProviderMetadata != 0 {
		return fmt.Errorf("cir: provider_metadata cannot be null")
	}
	if e.Kind != EventCompaction && (e.Replacement != nil || e.ReplacementComplete ||
		e.fieldPresence&(eventFieldReplacement|eventFieldReplacementComplete) != 0) {
		return fmt.Errorf("cir: replacement fields require compaction event")
	}
	if e.Kind != EventMessage && (e.AgentMessage || e.AgentAuthor != "" || e.AgentRecipient != "" ||
		e.fieldPresence&(eventFieldAgentMessage|eventFieldAgentAuthor|eventFieldAgentRecipient) != 0) {
		return fmt.Errorf("cir: agent message fields require message event")
	}
	if e.Kind == EventMessage && !e.AgentMessage && (e.AgentAuthor != "" || e.AgentRecipient != "" || e.Locked != nil ||
		e.fieldPresence&(eventFieldAgentAuthor|eventFieldAgentRecipient|eventFieldLocked) != 0) {
		return fmt.Errorf("cir: agent metadata requires agent_message true")
	}
	if e.Kind != EventMessage && e.Kind != EventReasoning && e.Kind != EventCompaction &&
		(e.Locked != nil || e.fieldPresence&eventFieldLocked != 0) {
		return fmt.Errorf("cir: locked state is not valid for %s event", e.Kind)
	}
	if e.fieldPresence&eventFieldLocked != 0 && e.Locked == nil {
		return fmt.Errorf("cir: locked state cannot be null")
	}
	return nil
}

// UnmarshalJSON deserializes a JSON in schema union form to an Event.
func (e *Event) UnmarshalJSON(data []byte) error {
	var w eventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = Event{
		Kind:                w.Kind,
		ID:                  w.ID,
		Ts:                  w.Ts,
		Seq:                 w.Seq,
		Role:                w.Role,
		Blocks:              w.Blocks,
		CallID:              w.CallID,
		ToolName:            w.ToolName,
		ProviderToolName:    w.ProviderToolName,
		Input:               w.Input,
		Status:              w.Status,
		Output:              w.Output,
		IsError:             w.IsError,
		Locked:              w.Locked,
		RedactedSummary:     w.RedactedSummary,
		CrossReplayable:     w.CrossReplayable,
		CompactSummary:      w.CompactSummary,
		AgentMessage:        w.AgentMessage,
		AgentAuthor:         w.AgentAuthor,
		AgentRecipient:      w.AgentRecipient,
		Replacement:         w.Replacement,
		ReplacementComplete: w.ReplacementComplete,
		ProviderMetadata:    w.ProviderMetadata,
		fieldPresence:       eventPresence(fields),
	}
	return nil
}

// ProviderMetadata is the normalized subset of provider-local item metadata
// required for same-provider replay. Unknown provider metadata keys make a
// compaction replacement incomplete instead of being silently discarded.
type ProviderMetadata struct {
	TurnID     string       `json:"turn_id,omitempty"`
	CreateTime *json.Number `json:"create_time,omitempty"`
}

// ContentBlock is a content block of a message event (domain model).
// type is fixed as "text" and can be extended to "image" and other types in the future.
type ContentBlock struct {
	// Type is the block type (currently "text").
	Type string `json:"type"`
	// Text is the text content.
	Text string `json:"text"`
}

// LockedBlob is a provider-locked original text (domain model).
// Claude preserves thinking.signature, and Codex preserves reasoning.encrypted_content.
//
// Core policy (immutable):
//   - blob is never parsed or modified (opaque preservation).
//   - a blob from one provider is never re-injected into another provider (cross-injection prohibition).
//   - during cross/memory replay, this block is preserved as an inert meta.
type LockedBlob struct {
	// Provider is the locking provider for this blob.
	Provider ProviderKind `json:"provider"`
	// Scheme is the locking method. Claude: "signature", Codex: "encrypted_content".
	Scheme string `json:"scheme"`
	// Blob is an opaque original text (signature/encrypted content). It is preserved without interpretation.
	Blob string `json:"blob"`
}
