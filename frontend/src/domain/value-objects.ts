/**
 * domain/value-objects: SPINE §4.1 Value Object TypeScript mirror.
 *
 * This file declares immutable value objects in pure types for the cxt domain.
 * It does not import any external packages — the end of dependency chain.
 *
 * Value Meaning (SPINE §4.1 + §8 TS rules):
 *   - string-literal union for enum mirror (prefix I forbidden).
 *   - All enum allowed values match the SPINE §4.1 contract 1:1.
 */

/**
 * Content-addressing basic unit.
 * Format: "sha256:<lowercase-hex-64-chars>" (e.g., "sha256:9f1c...").
 * Same ContentHash == same content (integrity dedup key). (SPINE §4.1 / DATA-MODEL §3.1)
 */
export type ContentHash = string;

/**
 * Capture/target provider types.
 * "claude"  — Claude Code (Anthropic).
 * "codex"   — Codex CLI (OpenAI).
 * "unknown" — Unverified provider (extension point). (SPINE §4.1 / cir.schema.json providerKind)
 */
export type ProviderKind = 'claude' | 'codex' | 'unknown';

/**
 * Restoration fidelity tier. (SPINE §5.5)
 * "full"          — Lossless restoration from original provider (reinjecting locked reasoning text).
 * "reconstructed" — Cross-provider restoration (reasoning inactive/summarized, text+toolcall preserved).
 * "memory"        — Distilled summary only (MemoryDigest). No transcript restoration.
 */
export type FidelityTier = 'full' | 'reconstructed' | 'memory';

/**
 * ref types. Unified representation of HEAD / branch / session / tag. (SPINE §4.1)
 * "head"   — Symbolic ref of the current checkout branch (exactly 1 per repo, name="HEAD").
 * "branch" — Context tip ref of the actual git branch (e.g., "main", "feat/x").
 * "session" — Session tips remaining after partial join within the same git branch.
 * "tag"    — Immutable label (e.g., "before-refactor", "v1-design").
 */
export type RefKind = 'head' | 'branch' | 'session' | 'tag';
