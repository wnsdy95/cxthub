package domain

import (
	"encoding/json"
	"fmt"
)

// This file defines the server-side domain types for CIR (Canonical Intermediate Representation).
// The schema definition is in schemas/cir.schema.json (completely separate from CLI declarations, wire-compatible).
//
// The backend does not know provider (claude/codex) raw formats. It receives CIR and stores the main body blob,
// recalculating the canonical_bytes hash for integrity verification (sync protocol).

// CIRDocument is a container for provider-independent normalized conversation expressions (cir.schema.json root).
//
// It consists of an envelope (session global metadata) + events (time-ordered sequence of event streams).
type CIRDocument struct {
	Envelope CIREnvelope `json:"envelope"`
	Events   []CIREvent  `json:"events"`
}

// CIREnvelope is session global metadata (cir.schema.json $defs/envelope).
//
// Note (data model OQ-5): Deterministic metadata like CapturedAt/SessionOriginID, if included in canonical_bytes,
// can cause deduplication to degrade if the same conversation has different IDs.
// v1 includes the entire envelope in the body integrity (policy is OQ-5).
type CIREnvelope struct {
	CIRVersion      string       `json:"cir_version"` // "1" or "2"
	SourceProvider  ProviderKind `json:"source_provider"`
	SourceModel     string       `json:"source_model"`
	CapturedAt      string       `json:"captured_at"` // RFC3339
	Cwd             string       `json:"cwd"`
	GitBranch       string       `json:"git_branch"`
	SessionOriginID string       `json:"session_origin_id"`
	Fidelity        FidelityTier `json:"fidelity"`
	// ContextTokens/OutputTokens are token usage statistics (0 = unobserved legacy capture, omitempty
	// means canonical bytes remain unchanged). context = context window size at session end,
	// output = total assistant output for the session.
	ContextTokens int `json:"context_tokens,omitempty"`
	OutputTokens  int `json:"output_tokens,omitempty"`
	// SourceModels are all models that appeared in the session (in order of appearance — SourceModel is the last used representative value).
	SourceModels []string `json:"source_models,omitempty"`
	// CompactionCount is the number of context compression (compact_boundary) events in a session transcript (0 = unobserved/no compression, omitempty means canonical bytes unchanged). Used for graph compression markers (◈).
	CompactionCount int `json:"compaction_count,omitempty"`
}

// EventKind is the tag for the CIR event union (cir.schema.json $defs/eventBase.kind).
type EventKind string

const (
	// EventTurn = turn boundary marker.
	EventTurn EventKind = "turn"
	// EventMessage = natural language message block.
	EventMessage EventKind = "message"
	// EventToolCall = tool call.
	EventToolCall EventKind = "tool_call"
	// EventToolResult = tool result.
	EventToolResult EventKind = "tool_result"
	// EventReasoning = reasoning event.
	EventReasoning EventKind = "reasoning"
	// EventCompaction = context compaction boundary or provider-locked active state.
	EventCompaction EventKind = "compaction"
)

// Role is the speaker role of a message/turn (cir.schema.json $defs/role).
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
)

// CIREvent is a flattened representation of an event union distinguished by the kind tag
// (cir.schema.json $defs/event = eventBase + oneOf).
//
// The server does not interpret event meaning and simply round-trips the body,
// so it does not force Go's separate types for JSON Schema's oneOf branches but represents
// the union of optional fields. seq in ascending order is the canonical sort order
// (canonical_bytes rule, data model).
type CIREvent struct {
	Kind EventKind `json:"kind"`
	ID   string    `json:"id,omitempty"`
	TS   string    `json:"ts,omitempty"` // RFC3339, optional
	Seq  int       `json:"seq"`
	// ProviderMetadata is a normalized provider-local replay identity. The
	// backend stores it opaquely and canonicalizes it with the event.
	ProviderMetadata *ProviderMetadata `json:"provider_metadata,omitempty"`
	// fieldPresence preserves optional zero-valued JSON fields for canonical
	// round trips and version/union validation. It is never serialized directly.
	fieldPresence eventFieldPresence

	// turn / message common: speaker role.
	Role Role `json:"role,omitempty"`

	// message: content block (v1 is text only).
	Blocks []ContentBlock `json:"blocks,omitempty"`
	// CompactSummary — agent compression-generated summary message indicator (CLI parity;
	// must be mirrored to prevent loss in server storage/serialization paths).
	CompactSummary bool `json:"compact_summary,omitempty"`
	// Codex multi-agent message metadata. Visible text stays in Blocks and the
	// opaque provider state uses Locked for same-provider replay only.
	AgentMessage   bool   `json:"agent_message,omitempty"`
	AgentAuthor    string `json:"agent_author,omitempty"`
	AgentRecipient string `json:"agent_recipient,omitempty"`
	// compaction boundary: provider effective replacement context. A non-nil
	// empty slice is meaningful and must survive canonical round trips.
	Replacement []CIREvent `json:"replacement,omitempty"`
	// False means an unknown provider replacement item was observed; consumers
	// must fail safe to the archival stream instead of replaying this partial projection.
	ReplacementComplete bool `json:"replacement_complete,omitempty"`

	// tool_call: Tool call field.
	CallID           string         `json:"call_id,omitempty"`
	ToolName         string         `json:"tool_name,omitempty"`          // Standard Name (provider compatibility rules Source of Truth)
	ProviderToolName string         `json:"provider_tool_name,omitempty"` // Original Provider Tool Name
	Input            map[string]any `json:"input,omitempty"`
	Status           string         `json:"status,omitempty"`

	// tool_result: Tool result field. Output is string/object/content block array (any to be preserved).
	Output  any   `json:"output,omitempty"`
	IsError *bool `json:"is_error,omitempty"`

	// reasoning: Reasoning event field.
	Locked          *LockedBlob `json:"locked,omitempty"`
	RedactedSummary string      `json:"redacted_summary,omitempty"`
	CrossReplayable *bool       `json:"cross_replayable,omitempty"` // Locked reasoning is always false
}

// MarshalJSON mirrors the CIR event union emitted by the CLI. The backend
// must preserve this exact shape when recomputing content hashes: required
// empty arrays/objects are part of canonical bytes and cannot be dropped by
// struct-level omitempty tags.
func (e CIREvent) MarshalJSON() ([]byte, error) {
	if err := e.validateKindFieldPresence(); err != nil {
		return nil, err
	}
	m := map[string]any{
		"kind": e.Kind,
		"seq":  e.Seq,
	}
	if e.ID != "" {
		m["id"] = e.ID
	}
	if e.TS != "" {
		m["ts"] = e.TS
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
			input = map[string]any{}
		}
		m["input"] = input
		if e.Status != "" {
			m["status"] = e.Status
		}
	case EventToolResult:
		m["call_id"] = e.CallID
		if e.Output == nil {
			m["output"] = ""
		} else {
			m["output"] = e.Output
		}
		if e.IsError != nil && *e.IsError {
			m["is_error"] = true
		}
	case EventReasoning:
		if e.Locked != nil {
			m["locked"] = e.Locked
		}
		if e.RedactedSummary != "" {
			m["redacted_summary"] = e.RedactedSummary
		}
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

func (e CIREvent) validateKindFieldPresence() error {
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

// UnmarshalJSON retains presence for optional zero values. Without this, a v1
// document could smuggle v2-only fields such as agent_message:false and have
// them silently disappear during canonicalization.
func (e *CIREvent) UnmarshalJSON(data []byte) error {
	type wire CIREvent
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = CIREvent(decoded)
	e.fieldPresence = eventPresence(fields)
	return nil
}

// ProviderMetadata mirrors the explicitly supported provider passthrough
// fields in CIR v2. Keeping this bounded avoids turning arbitrary provider
// objects into an unversioned escape hatch.
type ProviderMetadata struct {
	TurnID     string       `json:"turn_id,omitempty"`
	CreateTime *json.Number `json:"create_time,omitempty"`
}

// ContentBlock is a message content block (cir.schema.json $defs/contentBlock).
// v1 is the standard for type="text".
type ContentBlock struct {
	Type string `json:"type"` // fixed "text"
	Text string `json:"text"`
}

// LockedBlob is a provider-locked original text (cir.schema.json $defs/lockedBlob).
//
// claude=signature, codex=encrypted_content. Cross-play is not allowed and it is preserved without interpretation.
// (provider compatibility rules: opaque preservation, no interpretation, no cross-injection).
type LockedBlob struct {
	Provider ProviderKind `json:"provider"`
	Scheme   string       `json:"scheme"` // "signature" | "encrypted_content"
	Blob     string       `json:"blob"`   // opaque text (signature/encrypted content)
}

// SessionDoc is the snapshot body (CIR container) and is immutable, content-addressed.
// (data model Table, sync protocol). Exposed in the wire as docs/{hash}.
//
// Invariant H1: Hash == ContentHash(canonical_bytes(CIR)). In normal state, Snapshot.ID is equivalent.
type SessionDoc struct {
	Hash ContentHash `json:"hash"`
	CIR  CIRDocument `json:"cir"`
}
