/**
 * application/ports/session-gateway: Outbound port interface.
 *
 * The application layer abstracts the capabilities required by the external (backend cxt serve REST API).
 * The infrastructure layer implements it based on the fetch model, and the composition root injects it.
 *
 * FRONTEND-ARCHITECTURE §3.2 SessionGateway contract, 1:1.
 * SPINE §6.1 outbound port (read side) and §6.2 inbound use-case DTOs normalized for the frontend query side.
 * SYNC-PROTOCOL §2 REST surface, path constants stored in infrastructure/http/api-routes.ts.
 *
 * Dependency direction: application → domain only. Does not import infrastructure (DIP).
 */

import type {
  Repo,
  Branch,
  Snapshot,
  Ref,
  SessionDoc,
  MemoryDigest,
  Manifest,
} from '../../domain/index.js';

import type {
  ListInput,
  ListOutput,
  DiffInput,
  DiffOutput,
  ForkInput,
  ForkOutput,
  LoadInput,
  LoadOutput,
  CheckoutInput,
  CheckoutOutput,
  MemorizeInput,
  MemorizeOutput,
  SyncInput,
  SyncOutput,
} from '../dto.js';

/**
 * Gateway outbound port abstracting the backend cxt serve REST API.
 *
 * This interface establishes the layer boundary in the flow from presentation → application(use-case) → SessionGateway(ports) → infrastructure(implementation).
 *
 * All methods return a Promise, and errors are rejected with an Error.
 * HTTP error code mapping is handled by the infrastructure implementation (RestSessionGateway).
 */
export interface SessionGateway {
  // ── Retrieval (Read) ───────────────────────────────────────────────

  /**
   * List of accessible repos within team visibility.
   * SYNC-PROTOCOL §2.1 GET /api/v1/repos response.
   */
  listRepos(): Promise<Repo[]>;

  /**
   * List of branches for a repo (Ref kind=branch projection).
   * SYNC-PROTOCOL §2.3 GET /api/v1/repos/{repoId}/branches response.
   */
  listBranches(repoId: string): Promise<Branch[]>;

  /**
   * List of snapshots and refs for repo/branch.
   * SYNC-PROTOCOL §2.4 GET /api/v1/repos/{repoId}/snapshots?branch= response.
   * SPINE ListSessions / MCP session_list response.
   */
  listSessions(input: ListInput): Promise<ListOutput>;

/**
 * Retrieve a single snapshot metadata.
 * SYNC-PROTOCOL §2.4 GET /api/v1/repos/{repoId}/snapshots/{id} response.
 */
  getSnapshot(repoId: string, id: string): Promise<Snapshot>;

/**
 * Retrieve a single SessionDoc (CIR body).
 * Used for timeline/diff rendering and memory preview.
 * SYNC-PROTOCOL §2.5 GET /api/v1/repos/{repoId}/docs/{hash} response.
 */
  getDoc(repoId: string, docHash: string): Promise<SessionDoc>;

/**
 * Retrieve the repo's Manifest (ref list + snapshot index).
 * SYNC-PROTOCOL §2.2 GET /api/v1/repos/{repoId}/manifest response.
 */
  getManifest(repoId: string): Promise<Manifest>;

/**
 * Retrieve the MemoryDigest linked to a snapshot (if available).
 * SYNC-PROTOCOL §2.7 GET /api/v1/repos/{repoId}/memories/{snapshotId} response.
 */
  getMemory(repoId: string, snapshotId: string): Promise<MemoryDigest>;

  // ── Changes/Actions ─────────────────────────────────────────────

/**
 * Calculate the CIR event delta between two snapshots.
 * SYNC-PROTOCOL diff POST endpoint response.
 * SPINE DiffSnapshots / MCP session_diff response.
 */
  diff(input: DiffInput): Promise<DiffOutput>;

/**
 * Fork a new branch from a specified snapshot.
 * SYNC-PROTOCOL fork POST endpoint handling.
 * SPINE ForkSession / MCP session_fork handling.
 */
  fork(input: ForkInput): Promise<ForkOutput>;

/**
 * Restore a snapshot to the target provider session.
 * SYNC-PROTOCOL load POST endpoint handling.
 * SPINE LoadSession / MCP session_load handling.
 */
  load(input: LoadInput): Promise<LoadOutput>;

/**
 * Integrated checkout of fork (optional) + load.
 * If newBranch is specified, fork from from and then load; otherwise, simple load.
 * RECONCILIATION §D CheckoutSession / MCP session_checkout handling.
 * SYNC-PROTOCOL fork POST + load POST are sequentially synthesized.
 */
  checkout(input: CheckoutInput): Promise<CheckoutOutput>;

/**
 * [CLI exclusive — disabled on web]
 * Activate a session to distill a MemoryDigest and attach it to the current branch.
 * RECONCILIATION §D Memorize / MCP memorize / `cxt memorize` handling.
 * POST /api/v1/repos/{repoId}/memorize is not defined in openapi.yaml·backend http, so
 * it is not exposed as a web endpoint. The implementation always throws an error.
 */
  memorize(input: MemorizeInput): Promise<MemorizeOutput>;

/**
 * Push local snapshot/ref to the central server.
 * SYNC-PROTOCOL §3.2 push negotiation (negotiate→objects→refs) handling.
 * SPINE SyncRepo.Push / MCP sync_push handling.
 */
  syncPush(input: SyncInput): Promise<SyncOutput>;

/**
 * Pull central server changes to the local repository.
 * SYNC-PROTOCOL §3.3 pull negotiation (manifest→objects→ref merge) handling.
 * SPINE SyncRepo.Pull / MCP sync_pull handling.
 */
  syncPull(input: SyncInput): Promise<SyncOutput>;
}
