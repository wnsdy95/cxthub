/**
 * domain/entities: domain model Entity TypeScript mirror.
 *
 * Pure data shape (no logic). Only allow type guards/factory stubs.
 * Dependencies: value-objects.ts, cir.ts — maintain a dependency-free layer.
 *
 * Domain invariants (domain model / data model /):
 *   S-ID/H1 : Snapshot.id == Snapshot.docHash == ContentHash(canonical(SessionDoc.cir)).
 *   S1      : Snapshot is immutable after storage.
 *   REF1    : Ref.target is a real Snapshot.id; symbolic HEAD is a real branch ref.
 *   REF2    : Tag ref is immutable (force re-tagging is delete+create only).
 *   REF3    : HEAD is exactly 1 per repo, name="HEAD".
 *   B1      : Branch.head == Ref(branch, name).target (ref is the source of truth).
 *   M1      : All targets in Manifest.refs are included in snapshotIndex.
 *   M2      : Manifest.version is monotonically increasing.
 */

import type { ContentHash, ProviderKind, FidelityTier, RefKind } from './value-objects.js';
import type { CIRDocument } from './cir.js';

// ── TeamIdentity ────────────────────────────────────────────────

/**
 * Author identifier value object. Embedded in snapshot author. (domain model TeamIdentity)
 */
export interface TeamIdentity {
/** Person name. */
  name: string;
/** Email (ownership + audit log). */
  email: string;
/** Team identifier (visibility boundary unit). */
  team: string;
}

// ── Repo ────────────────────────────────────────────────────────

/**
 * Root of session storage space. One per detected code repository. (domain model Repo)
 *
 * Invariants:
 *   R1: id is stable across repo lifetime (remote promotion is migration).
 *   R2: defaultBranch is the actual branch ref name.
 *
 * id derivation (data model):
 *   git remote URL exists → ContentHash of normalize(remoteUrl).
 *   otherwise → ContentHash of normalize(localPath absolute path).
 */
export interface Repo {
/** Normalized remote URL or ContentHash of cwd fallback. */
  id: string;
/** Git remote URL (non-empty on server side; empty for local-only repos). */
  remoteUrl: string;
/** Local absolute path (always empty in server-side responses). */
  localPath: string;
/** Default branch name (must match existing branch ref). */
  defaultBranch: string;
}

// ── Branch ──────────────────────────────────────────────────────

/**
 * Session line (code git branch name reused). (domain model Branch)
 *
 * Invariant B1: head == Ref(kind=branch, name).target (ref is source of truth, Branch is read-only view).
 */
export interface Branch {
/** Branch name (code git branch name directly reused). */
  name: string;
/** repo id. */
  repoId: string;
/**
 * Latest snapshot ContentHash.
 * Source of truth is Ref(branch, name).target — Branch.head is its projection (read view).
 */
  head: ContentHash;
}

// ── Snapshot ────────────────────────────────────────────────────

/**
 * commit (immutable, content-addressed). (domain model Snapshot / data model)
 *
 * Core invariant (S-ID/H1):
 *   id == docHash == ContentHash(canonical_bytes(SessionDoc.cir))
 *   — The same conversation body always has the same id → Automatic deduplication and integrity verification possible.
 *
 * S-ID implication: commit meta (parents, message, author, createdAt) is not included in id calculation
 * → Same body + different meta can share the same id (data model, OQ-1).
 *
 * Additional invariants:
 *   S1: Once stored, no field can be changed.
 *   S2: docHash must point to a SessionDoc that exists in the store.
 *   S3: All hashes in parents must exist in the same repo (root is an empty array).
 *   S4: Parents links form a DAG, no cycles allowed.
 *   S5: branch is the branch label at creation time (not ownership — Ref has ownership).
 */
export interface Snapshot {
/**
 * ContentHash(canonical_bytes(CIR)) — Immutable unique identifier.
 * Format: "sha256:<lowercase-hex-64>".
 */
  id: ContentHash;
/** repo id. */
  repoId: string;
/** Branch label at creation time (not ownership — Ref has ownership). */
  branch: string;
/**
 * DAG parent snapshot ID list.
 * Root snapshot is an empty array. Possible to have 2 or more during fork/merge.
 */
  parents: ContentHash[];
/**
 * ContentHash of the SessionDoc being referenced.
 * Invariant H1: id == docHash.
 */
  docHash: ContentHash;
/** Original capture provider. */
  provider: ProviderKind;
/** Recovery fidelity tier of this snapshot. */
  fidelity: FidelityTier;
/** Snapshot description message (corresponds to git commit message). */
  message: string;
/** Author identifier. */
  author: TeamIdentity;
/** Creation time (RFC3339 — Go time.Time JSON serialization). */
  createdAt: string;
}

// ── Ref ─────────────────────────────────────────────────────────

/**
 * Variable pointer (HEAD / branch / tag unified representation). (domain model Ref / data model)
 *
 * Three kinds:
 *   kind="head",   name="HEAD"     — Symbolic ref. symbolic=Current branch name ("" if detached).
 *   kind="branch", name=<branch>   — Session line tip. git refs/heads/<name> corresponds.
 *   kind="tag",    name=<tag>      — Immutable label. target is changed by REF2.
 *
 * Invariants:
 *   REF1: target (directly pointed) is existing Snapshot.id; symbolic HEAD is existing branch ref.
 *   REF2: tag ref is immutable (force re-tagging = delete+create).
 *   REF3: repo has exactly 1 kind=head, name="HEAD".
 *   REF4: branch tip advancement is only allowed via fast-forward or explicit (fork/conflict handling).
 */
export interface Ref {
  kind: RefKind;
/** Ref name (e.g., "main", "feat/x", "before-refactor"; HEAD is "HEAD"). */
  name: string;
/** Owning repo id. */
  repoId: string;
/** Snapshot id being pointed to. Empty string for symbolic HEAD. */
  target: ContentHash;
/**
 * Branch name that symbolic HEAD is pointing to.
 * Empty string if directly pointed (detached HEAD) or branch/tag.
 */
  symbolic: string;
}

// ── SessionDoc ──────────────────────────────────────────────────

/**
 * CIR container — normalized conversation body (immutable). (domain model SessionDoc)
 *
 * Invariant H1: hash == ContentHash(canonical_bytes(cir)).
 * Snapshot.docHash points to this hash.
 */
export interface SessionDoc {
/** Original content hash. Invariant H1 verifiable. */
  hash: ContentHash;
/** Normalized CIR body. */
  cir: CIRDocument;
}

// ── MemoryDigest ────────────────────────────────────────────────

/**
 * Distilled memory (for memory-form load). (domain model MemoryDigest)
 *
 * Derived from Snapshot. snapshotId links to Snapshot 1:1.
 * Used for CLAUDE.md(claude) / AGENTS.md(codex) injection.
 */
export interface MemoryDigest {
/** Original snapshot id. */
  snapshotId: ContentHash;
/** Previous attachment for the same snapshot (causal fast-forward parent). */
  previousMemoryHash?: ContentHash;
/** Human-readable summary body (for provider memory injection). */
  summary: string;
/** Core facts list. */
  keyFacts: string[];
/** Open task list. */
  openTasks: string[];
/** Injection target format hint (used for provider format determination). */
  provider: ProviderKind;
/** Snapshot-scoped contributions used to union natural and graft lineages. */
  fragments?: MemoryFragment[];
/** Exact graft register projected when this digest was attached. */
  graftCoverage?: MemoryGraftCoverage;
}

export interface MemoryFragment {
  sourceSnapshot: ContentHash;
  summary?: string;
  keyFacts?: string[];
  openTasks?: string[];
  tasksAuthoritative?: boolean;
}

export interface MemoryGraftCoverage {
  projectionVersion: number;
  projectionComplete: boolean;
  lineageFingerprint?: ContentHash;
  graftSeq: number;
  graftParents?: ContentHash[];
  pinnedSources?: ContentHash[];
}

// ── Manifest ────────────────────────────────────────────────────

/**
 * Repository unit metadata index — catalog for push/pull negotiations. (domain model Manifest / data model)
 *
 * Invariants:
 *   M1: All targets in refs must be included in snapshotIndex (dangling refs forbidden).
 *   M2: Version must monotonically increase with each update (optimistic locking version).
 */
export interface Manifest {
/** Owning repo id. */
  repoId: string;
/** All mutable pointers (HEAD / branch / tag). */
  refs: Ref[];
/** List of owned snapshot IDs (push/pull difference negotiation's have set). */
  snapshotIndex: ContentHash[];
/** Current causal memory pointer keyed by snapshot ID (absent on legacy peers). */
  memoryAttachments?: Record<ContentHash, ContentHash>;
/** Optimistic lock version (monotonically increasing). */
  version: number;
/** Last update timestamp (RFC3339). */
  updatedAt: string;
}

// ── Type guard stub ──────────────────────────────────────────────

/**
 * ContentHash format validation type guard stub.
 * Format: "sha256:<lowercase-hex-64>".
 * Contract scaffold; behavior is implemented by the application and infrastructure layers.
 */
export function isContentHash(value: unknown): value is ContentHash {
  throw new Error('not implemented');
}

/**
 * Snapshot ID == docHash invariant (H1) validation helper stub.
 * For display/integrity badge purposes. Actual canonical bytes recalculation in subsequent implementation.
 */
export function verifyContentAddress(_snapshot: Snapshot, _doc: SessionDoc): boolean {
  throw new Error('not implemented');
}
