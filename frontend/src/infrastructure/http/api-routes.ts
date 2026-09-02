/**
 * infrastructure/http/api-routes: Backend REST endpoint path constants.
 *
 * Fixed the REST surface as constants according to sync protocol
 * Paths are defined in sync protocol and frontend architecture table (compatibility rules).
 * The OpenAPI schema is the authoritative route contract.
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

/** Backend REST API path constants. 1:1 correspondence with sync protocol */
export const ApiRoutes = {
  // ── Repos ─────────────────────────────────────────────

/** GET /api/v1/repos — Team visibility repo list. */
  repos: (): string =>
    '/api/v1/repos',

/** GET /api/v1/repos/{repoId} — Single repo retrieval. */
  repo: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}`,

  // ── Manifest ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/manifest — Manifest (negotiation catalog). */
  manifest: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/manifest`,

  // ── Branches ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/branches — Branch list (Ref kind=branch projection). */
  branches: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/branches`,

  // ── Snapshots ─────────────────────────────────────────

  /**
   * GET /api/v1/repos/{repoId}/snapshots — list snapshots (query: ?branch=<name>).
   * Snapshots are created only through the push endpoint, not an individual POST (sync protocol).
   */
  snapshots: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/snapshots`,

/** GET /api/v1/repos/{repoId}/snapshots/{id} — Single snapshot metadata. */
  snapshot: (repoId: string, id: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/snapshots/${encodeURIComponent(id)}`,

  // ── Docs (blobs) ──────────────────────────────────────

/** GET /api/v1/repos/{repoId}/docs/{hash} — Single SessionDoc (CIR body). */
  doc: (repoId: string, hash: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/docs/${encodeURIComponent(hash)}`,

  // ── Refs ──────────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/refs — Full ref list. */
  refs: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/refs`,

/**
 * PUT /api/v1/repos/{repoId}/refs/{kind}/{name} — Ref move (CAS).
 * Step C of push workflow (sync protocol).
 */
  ref: (repoId: string, kind: string, name: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/refs/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`,

  // ── Memories ──────────────────────────────────────────

/** GET /api/v1/repos/{repoId}/memories/{snapshotId} — MemoryDigest retrieval. */
  memory: (repoId: string, snapshotId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/memories/${encodeURIComponent(snapshotId)}`,

  // ── Push Negotiation Endpoints ──────────────────────────────

/** POST /api/v1/repos/{repoId}/push/negotiate — Step A: Missing Hash Negotiation. */
  pushNegotiate: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/push/negotiate`,

/** POST /api/v1/repos/{repoId}/push/objects — Step B: Object Batch Upload. */
  pushObjects: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/push/objects`,

  // ── Pull Negotiation Endpoints ──────────────────────────────

/** POST /api/v1/repos/{repoId}/pull/objects — Step B: Missing Object Batch Download. */
  pullObjects: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/pull/objects`,

  // ── Action Endpoints (Frontend Enhancement: diff/fork/load) ──────────

/**
 * POST /api/v1/repos/{repoId}/diff — Snapshot CIR event delta.
 * DiffSnapshots REST response.
 * (frontend architecture Proposed Path)
 */
  diff: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/diff`,

/**
 * POST /api/v1/repos/{repoId}/fork — Branch fork from snapshot.
 * ForkSession REST response.
 */
  fork: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/fork`,

/**
 * POST /api/v1/repos/{repoId}/load — Restore snapshot to target provider session.
 * LoadSession REST response.
 */
  load: (repoId: string): string =>
    `/api/v1/repos/${encodeURIComponent(repoId)}/load`,

} as const;
// NOTE: POST /api/v1/repos/{repoId}/memorize is a CLI-only action (`cxt memorize`) and is not exposed as a web endpoint. This path is also absent from openapi.yaml and backend HTTP handlers.
