/**
 * infrastructure/http/api-routes: Backend REST endpoint path constants.
 *
 * Fixed the REST surface as constants according to SYNC-PROTOCOL §2.
 * Paths are defined in SYNC-PROTOCOL §2 and FRONTEND-ARCHITECTURE §4.3 table (RECONCILIATION §G).
 * Open Question 1(§7) is resolved according to the §4.3/SYNC-PROTOCOL standard.
 *
 * The backend adapters/delivery HTTP handlers and this file must share the same paths.
 *
 * ── API base URL (build-time environment variable) ─────────────────────────────────────────
 * The frontend is deployed on a CDN/Vercel or other hosting (CORS assumed), so the backend URL is
 * injected as a build-time environment variable. Example:
 *   VITE_API_BASE_URL=https://cxt-api.example.com   (Vite)
 *   NEXT_PUBLIC_API_BASE_URL=https://cxt-api.example.com  (Next.js)
 * This file's path constants return only the path (not the full URL).
 * The base URL is read from the environment variable in HttpClientConfig.baseUrl in
 * composition root presentation/bootstrap/container.ts.
 *
 * All paths start with /api/v1.
 *
 * Usage examples:
 *   ApiRoutes.repos()             → "/api/v1/repos"
 *   ApiRoutes.snapshots(repoId)   → "/api/v1/repos/{repoId}/snapshots"
 *   ApiRoutes.snapshot(repoId, id) → "/api/v1/repos/{repoId}/snapshots/{id}"
 */

/** Backend REST API path constants. 1:1 correspondence with SYNC-PROTOCOL §2. */
export const ApiRoutes = {
  // ── §2.1 Repos ─────────────────────────────────────────────

/** GET /api/v1/repos — Team visibility repo list. */
  repos: (): string =>
    '/api/v1/repos',

/** GET /api/v1/repos/{repoId} — Single repo retrieval. */
  repo: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}`,

  // ── §2.2 Manifest ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/manifest — Manifest (negotiation catalog). */
  manifest: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/manifest`,

  // ── §2.3 Branches ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/branches — Branch list (Ref kind=branch projection). */
  branches: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/branches`,

  // ── §2.4 Snapshots ─────────────────────────────────────────

  /**
   * GET /api/v1/repos/{repoId}/snapshots — list snapshots (query: ?branch=<name>).
   * Snapshots are created only through the push endpoint, not an individual POST (SYNC-PROTOCOL §2.4).
   */
  snapshots: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/snapshots`,

/** GET /api/v1/repos/{repoId}/snapshots/{id} — Single snapshot metadata. */
  snapshot: (repoId: string, id: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/snapshots/${encodeURIComponent(id)}`,

  // ── §2.5 Docs (blobs) ──────────────────────────────────────

/** GET /api/v1/repos/{repoId}/docs/{hash} — Single SessionDoc (CIR body). */
  doc: (repoId: string, hash: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/docs/${encodeURIComponent(hash)}`,

  // ── §2.6 Refs ──────────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/refs — Full ref list. */
  refs: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/refs`,

/**
 * PUT /api/v1/repos/{repoId}/refs/{kind}/{name} — Ref move (CAS).
 * Step C of push workflow (SYNC-PROTOCOL §3.2).
 */
  ref: (repoId: string, kind: string, name: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/refs/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`,

  // ── §2.7 Memories ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/memories/{snapshotId} — MemoryDigest retrieval. */
  memory: (repoId: string, snapshotId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/memories/${encodeURIComponent(snapshotId)}`,

  // ── §3.2 Push Negotiation Endpoints ──────────────────────────────

/** POST /api/v1/repos/{repoId}/push/negotiate — Step A: Missing Hash Negotiation. */
  pushNegotiate: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/push/negotiate`,

/** POST /api/v1/repos/{repoId}/push/objects — Step B: Object Batch Upload. */
  pushObjects: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/push/objects`,

  // ── §3.3 Pull Negotiation Endpoints ──────────────────────────────

/** POST /api/v1/repos/{repoId}/pull/objects — Step B: Missing Object Batch Download. */
  pullObjects: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/pull/objects`,

  // ── Action Endpoints (Frontend Enhancement: diff/fork/load) ──────────

/**
 * POST /api/v1/repos/{repoId}/diff — Snapshot CIR event delta.
 * SPINE DiffSnapshots / MCP session_diff response.
 * (FRONTEND-ARCHITECTURE §4.3 Proposed Path)
 */
  diff: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/diff`,

/**
 * POST /api/v1/repos/{repoId}/fork — Branch fork from snapshot.
 * SPINE ForkSession / MCP session_fork response.
 */
  fork: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/fork`,

/**
 * POST /api/v1/repos/{repoId}/load — Restore snapshot to target provider session.
 * SPINE LoadSession / MCP session_load response.
 */
  load: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/load`,

} as const;
// NOTE: POST /api/v1/repos/{repoId}/memorize is a CLI-only action (`cxt memorize`) and is not exposed as a web endpoint. This path is also absent from openapi.yaml and backend HTTP handlers.
