/**
 * domain/cir: CIR v1/v2 (Canonical Intermediate Representation) TypeScript mirror.
 *
 * domain model + schemas/cir.schema.json 1:1 mapping.
 * Backend serializes JSON with snake_case field names, so this file declares them the same way.
 * (No difference in notation to be absorbed in infrastructure/mappers.ts — CIR has snake_case schema source).
 *
 * Dependency: import only from value-objects.ts — maintain a dependency-free layer.
 */

import type { ProviderKind, FidelityTier } from './value-objects.js';

// ── Common Helper Types ──────────────────────────────────────────────

/**
 * Conversation participant role. (domain model turn / message event)
 * "user" | "assistant" | "system" | "developer"
 */
export type Role = 'user' | 'assistant' | 'system' | 'developer';

// ── Envelope ────────────────────────────────────────────────────

/**
 * CIR session global metadata. Required header for all CIRDocument. (domain model / schema envelope)
 *
 * Field names follow the snake_case convention from the schema source.
 */
export interface Envelope {
/** CIR schema version. Compaction and multi-agent fields require v2. */
  cir_version: '1' | '2';
/** Original capture provider. */
  source_provider: ProviderKind;
/** Original model identifier (e.g., "claude-opus-4-8", "gpt-..."). */
  source_model: string;
/** Capture timestamp (RFC3339). */
  captured_at: string;
/** Absolute working directory at capture time. */
  cwd: string;
/**
 * Code git branch at capture time.
 * Claude fills the record's gitBranch field, Codex populates via gitctx lookup.
 */
  git_branch: string;
/** Original session identifier (Claude sessionId / Codex rollout UUID). */
  session_origin_id: string;
/** Fidelity tier of this CIR document as a whole. */
  fidelity: FidelityTier;
/** Last observed provider input-context usage. Omitted for legacy captures. */
  context_tokens?: number;
/** Cumulative assistant output usage. Omitted for legacy captures. */
  output_tokens?: number;
/** Models observed in first-appearance order. */
  source_models?: string[];
/** Number of provider context compactions observed in this archive. */
  compaction_count?: number;
}

// ── ContentBlock ────────────────────────────────────────────────

/**
 * Elements of message.blocks. v1 is limited to "text" type.
 * Extensions like images are outside v1 scope (type extensible). (domain model contentBlock)
 */
export interface ContentBlock {
  type: 'text';
  text: string;
}

// ── LockedBlob ──────────────────────────────────────────────────

/**
 * Provider-locked original text container to preserve.
 * Cross-provider playback not allowed — preserve as opaque blob only. (domain model)
 *
 * Provider-specific schemes:
 *   claude → "signature"         (thinking.signature field)
 *   codex  → "encrypted_content" (reasoning.encrypted_content field)
 */
export interface LockedBlob {
/** Provider that locked (created) this blob. */
  provider: ProviderKind;
/** Locking method. */
  scheme: 'signature' | 'encrypted_content';
/** Opaque original text (signature/encrypted content). No interpretation, preserve only. */
  blob: string;
}

/** Provider-local replay metadata retained only for same-provider full loads. */
export interface ProviderMetadata {
  turn_id?: string;
  create_time?: number;
}

// ── Event types (tag union) ────────────────────────────────────

/**
 * Common fields for all Events. (domain model eventBase)
 * Not exposed in public interfaces — declared directly in each specific event.
 */
interface EventBase {
/** Original identifier / creation ID (optional). */
  id?: string;
/** Event timestamp (RFC3339, optional). */
  ts?: string;
/** Normalized order. Basis for ascending sorting. (domain model) */
  seq: number;
/** Explicitly modeled provider-local replay identity (CIR v2). */
  provider_metadata?: ProviderMetadata;
}

/**
 * Turn boundary marker (UI / turn metadata). (domain model kind="turn")
 */
export interface TurnEvent extends EventBase {
  kind: 'turn';
  role: Role;
}

/**
 * Natural language message block. (domain model kind="message")
 * claude: user/assistant message content.
 * codex: response_item type=message.
 */
export interface MessageEvent extends EventBase {
  kind: 'message';
  role: Role;
  blocks: ContentBlock[];
/** Provider-generated context summary marker. */
  compact_summary?: boolean;
/** Codex multi-agent message retained in provider active context. */
  agent_message?: boolean;
/** Provider-local author identity for an agent message. */
  agent_author?: string;
/** Provider-local recipient identity for an agent message. */
  agent_recipient?: string;
/** Opaque same-provider state attached to an agent message. */
  locked?: LockedBlob;
}

/**
 * Tool call event. (domain model kind="tool_call")
 * claude: content block type="tool_use".
 * codex: response_item type="function_call" | "custom_tool_call" | "web_search_call".
 */
export interface ToolCallEvent extends EventBase {
  kind: 'tool_call';
/** Original session tool call identifier. */
  call_id: string;
/** Provider-independent canonical tool name (mapped in codec). */
  tool_name: string;
/** Original provider's tool name (preserved, optional). */
  provider_tool_name?: string;
/** Tool call input (provider-independent format). */
  input: Record<string, unknown>;
/** Tool execution status (optional; codex custom_tool_call status field). */
  status?: string;
}

/**
 * Tool execution result event. (domain model kind="tool_result")
 * claude: user-side tool_result block.
 * codex: response_item type="function_call_output" | "custom_tool_call_output".
 */
export interface ToolResultEvent extends EventBase {
  kind: 'tool_result';
/** Corresponding ToolCallEvent.call_id. */
  call_id: string;
/** Tool execution output (string, struct, or provider content block array). */
  output: string | Record<string, unknown> | unknown[];
/** Error flag (optional; true if error result). */
  is_error?: boolean;
}

/**
 * Reasoning event. Provider lock handling. (domain model kind="reasoning")
 *
 * Invariants:
 *   - cross_replayable is always false in locked reasoning.
 *   - Same-provider load: locked.blob original re-injected (full).
 *   - Cross-provider load: locked is meta-preserved (inactive), redacted_summary only re-downgraded → fidelity="reconstructed".
 *   - Memory load: only redacted_summary used → fidelity="memory".
 */
export interface ReasoningEvent extends EventBase {
  kind: 'reasoning';
/** Provider lock original (optional; preserved only, cross-replay not allowed). */
  locked?: LockedBlob;
/** Plain summary (cross-provider playback / memory, optional). */
  redacted_summary?: string;
/** Locked reasoning is cross-playback impossible — always false. */
  cross_replayable: false;
}

/**
 * Archival context-compaction boundary. The replacement is authoritative only
 * when replacement_complete is true; false makes consumers replay the archive.
 */
export interface CompactionBoundaryEvent extends EventBase {
  kind: 'compaction';
  replacement: CIREvent[];
  replacement_complete: boolean;
  locked?: never;
}

/** Provider-locked Codex compaction state inside a replacement history. */
export interface CompactionLockedEvent extends EventBase {
  kind: 'compaction';
  locked: LockedBlob;
  replacement?: never;
  replacement_complete?: never;
}

export type CompactionEvent = CompactionBoundaryEvent | CompactionLockedEvent;

/**
 * kind tag union — all event types of CIR v1/v2. (domain model event oneOf)
 */
export type CIREvent =
  | TurnEvent
  | MessageEvent
  | ToolCallEvent
  | ToolResultEvent
  | ReasoningEvent
  | CompactionEvent;

// ── CIRDocument (root) ──────────────────────────────────────────

/**
 * CIR v1/v2 root document.
 * Invariant: events are sorted in ascending order by seq. (domain model / schema root)
 */
export interface CIRDocument {
/** Session global metadata. */
  envelope: Envelope;
/** Event stream sorted by time (seq in ascending order is the normal state). */
  events: CIREvent[];
}
