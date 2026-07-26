/**
 * application/dto: use-case input/output DTO.
 *
 * SPINE §6.2 inbound DTO TypeScript mirror (camelCase).
 * Declares only the shape of input (Input) / output (Output) for each use-case — no logic.
 * Dependency: domain layer only.
 */

import type {
  ContentHash,
  ProviderKind,
  FidelityTier,
  Snapshot,
  Ref,
  TeamIdentity,
} from '../domain/index.js';

// ── ListSessions (SPINE ListSessions / MCP session_list) ─────────

/** Snapshot/ref list query input. */
export interface ListInput {
/** Repository ID to query. */
  repoId: string;
/** Filter branch name (empty string for all branches). */
  branch: string;
}

/** Snapshot/ref list query output. */
export interface ListOutput {
/** Snapshot list for the repo/branch (createdAt descending recommended). */
  snapshots: Snapshot[];
/** List of refs in this repo (HEAD/branch/tag combined). */
  refs: Ref[];
}

// ── DiffSnapshots (SPINE DiffSnapshots / MCP session_diff) ──────

/** Diff input for two snapshots. */
export interface DiffInput {
/** ID of the containing repo. */
  repoId: string;
/** Base snapshot ID (left / before). */
  left: ContentHash;
/** Target snapshot ID (right / after). */
  right: ContentHash;
}

/**
 * Single diff entry. (SPINE DiffEntry)
 * op: "add" | "remove" | "modify" etc. independent operation name of the provider.
 */
export interface DiffEntry {
  op: string;
/** CIR seq. of this event. */
  seq: number;
/** Summary of changes for human readers. */
  summary: string;
}

/** Diff output of two snapshots. */
export interface DiffOutput {
  changes: DiffEntry[];
}

// ── ForkSession (SPINE ForkSession / MCP session_fork) ──────────

/** Fork input. */
export interface ForkInput {
/** ID of the associated repo. */
  repoId: string;
/** Snapshot ID as the base for the branch (this snapshot becomes the new branch tip). */
  fromSnapshot: ContentHash;
/** New branch name (must not exist — invariant F2). */
  newBranch: string;
/** Fork author identity. */
  author: TeamIdentity;
}

/** Fork output. */
export interface ForkOutput {
/** Generated branch name. */
  branch: string;
/** Current head of the new branch (== fromSnapshot). */
  head: ContentHash;
}

// ── LoadSession (SPINE LoadSession / MCP session_load) ──────────

/** Session load (restore) input. */
export interface LoadInput {
/** Repository ID. */
  repoId: string;
/** Load reference name (branch name, tag name, or "HEAD"). */
  ref: string;
/** Recovery target provider (Claude/Codex session file format determination). */
  targetProvider: ProviderKind;
/** Recovery fidelity mode (full/reconstructed/memory). */
  mode: FidelityTier;
/** CWD to record the target provider session file. */
  cwd: string;
}

/** Session load (recovery) output. */
export interface LoadOutput {
/** Absolute path to the recorded session file. */
  writtenPath: string;
/** Actual fidelity applied (may differ from requested mode — cross-provider fallback). */
  fidelity: FidelityTier;
/**
 * Target provider native resume command (e.g., "claude --resume <id>").
 * Filled by SessionMaterializer in full mode on success. Empty string in memory mode or fallback.
 * RECONCILIATION §C.2 LoadOutput.ResumeCmd handling.
 */
  resumeCmd: string;
}

// ── CheckoutSession (RECONCILIATION §D / MCP session_checkout) ──

/**
 * Checkout input.
 * If newBranch != "", forks and loads (cxt checkout -b <new>).
 * If newBranch == "", simply loads (cxt checkout <ref>).
 * RECONCILIATION §D CheckoutInput handling.
 */
export interface CheckoutInput {
/** Owner repo id. */
  repoId: string;
/** Base ref name (branch name, snapshot id, "HEAD"). */
  from: string;
/**
 * New branch name.
 * If empty, perform a simple load. If a value is provided, fork and load (-b flag handling).
 */
  newBranch: string;
/** Recovery target provider. */
  targetProvider: ProviderKind;
/** Recovery fidelity mode. */
  mode: FidelityTier;
/** CWD to record the target provider session file. */
  cwd: string;
}

/** Checkout output. RECONCILIATION §D CheckoutOutput handling. */
export interface CheckoutOutput {
/** Current active branch name. */
  branch: string;
/** Current head hash of the branch. */
  head: ContentHash;
/** Absolute path of the recorded session file (load step). */
  writtenPath: string;
/** Target provider native resume command (e.g., "claude --resume <id>"). */
  resumeCmd: string;
/** Actual applied fidelity. */
  fidelity: FidelityTier;
}

// ── Memorize (RECONCILIATION §D / MCP memorize) ─────────────────

/**
 * memorize input.
 * Reconcile the active session to attach a MemoryDigest to the current branch.
 * RECONCILIATION §D MemorizeInput response.
 */
export interface MemorizeInput {
/** Owning repo id. */
  repoId: string;
/** Target provider (decides which provider's native memory to absorb). */
  provider: ProviderKind;
/** Working directory (native memory file search base). */
  cwd: string;
}

/** Memorize output. RECONCILIATION §D MemorizeOutput handling. */
export interface MemorizeOutput {
/** Snapshot identifier hash (raw DocHash). */
  snapshotId: ContentHash;
/** Generated MemoryDigest hash. */
  memoryHash: ContentHash;
/** Whether MemoryDigest is attached to the current branch. */
  attached: boolean;
}

// ── SyncRepo (SPINE SyncRepo / MCP sync_push, sync_pull) ────────

/** Push/pull input. */
export interface SyncInput {
/** Synchronization target repo ID. */
  repoId: string;
}

/** Push/pull output. */
export interface SyncOutput {
/** Number of snapshots uploaded to the server. */
  pushed: number;
/** Number of snapshots downloaded from the server. */
  pulled: number;
/**
 * List of new refs generated by this sync.
 * Branches preserved fork branches ("<branch>--fork-<shortid>") may be included.
 */
  newRefs: Ref[];
}
