/**
 * domain/cir: CIR v1 (Canonical Intermediate Representation) TypeScript mirror.
 *
 * SPINE §5 + schemas/cir.schema.json 1:1 mapping.
 * Backend serializes JSON with snake_case field names, so this file declares them the same way.
 * (No difference in notation to be absorbed in infrastructure/mappers.ts — CIR has snake_case schema source).
 *
 * Dependency: import only from value-objects.ts — maintain a dependency-free layer.
 */

import type { ProviderKind, FidelityTier } from './value-objects.js';

// ── Common Helper Types ──────────────────────────────────────────────

/**
 * Conversation participant role. (SPINE §5.3 turn / message event)
 * "user" | "assistant" | "system" | "developer"
 */
export type Role = 'user' | 'assistant' | 'system' | 'developer';

// ── Envelope ────────────────────────────────────────────────────

/**
 * CIR session global metadata. Required header for all CIRDocument. (SPINE §5.2 / schema envelope)
 *
 * Field names follow the snake_case convention from the schema source.
 */
export interface Envelope {
/** CIR schema version. Current const "1". */
  cir_version: '1';
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
 * (RESEARCH-FINDINGS: Confirm gitBranch is an intrinsic field in Claude records)
 */
  git_branch: string;
/** Original session identifier (Claude sessionId / Codex rollout UUID). */
  session_origin_id: string;
/** Fidelity tier of this CIR document as a whole. */
  fidelity: FidelityTier;
}

// ── ContentBlock ────────────────────────────────────────────────

/**
 * Elements of message.blocks. v1 is limited to "text" type.
 * Extensions like images are outside v1 scope (type extensible). (SPINE §5.3 contentBlock)
 */
export interface ContentBlock {
  type: 'text';
  text: string;
}

// ── LockedBlob ──────────────────────────────────────────────────

/**
 * Provider-locked original text container to preserve.
 * Cross-provider playback not allowed — preserve as opaque blob only. (SPINE §5.4)
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

// ── Event types (tag union) ────────────────────────────────────

/**
 * Common fields for all Events. (SPINE §5.3 eventBase)
 * Not exposed in public interfaces — declared directly in each specific event.
 */
interface EventBase {
/** Original identifier / creation ID (optional). */
  id?: string;
/** Event timestamp (RFC3339, optional). */
  ts?: string;
/** Normalized order. Basis for ascending sorting. (SPINE §5.3) */
  seq: number;
}

/**
 * Turn boundary marker (UI / turn metadata). (SPINE §5.3 kind="turn")
 */
export interface TurnEvent extends EventBase {
  kind: 'turn';
  role: Role;
}

/**
 * Natural language message block. (SPINE §5.3 kind="message")
 * claude: user/assistant message content.
 * codex: response_item type=message.
 */
export interface MessageEvent extends EventBase {
  kind: 'message';
  role: Role;
  blocks: ContentBlock[];
}

/**
 * Tool call event. (SPINE §5.3 kind="tool_call")
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
 * Tool execution result event. (SPINE §5.3 kind="tool_result")
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
 * Reasoning event. Provider lock handling. (SPINE §5.4 kind="reasoning")
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
 * kind tag union — all event types of CIR v1. (SPINE §5.3 event oneOf)
 */
export type CIREvent =
  | TurnEvent
  | MessageEvent
  | ToolCallEvent
  | ToolResultEvent
  | ReasoningEvent;

// ── CIRDocument (root) ──────────────────────────────────────────

/**
 * CIR v1 root document.
 * Invariant: events are sorted in ascending order by seq. (SPINE §5.1 / schema root)
 */
export interface CIRDocument {
/** Session global metadata. */
  envelope: Envelope;
/** Event stream sorted by time (seq in ascending order is the normal state). */
  events: CIREvent[];
}
