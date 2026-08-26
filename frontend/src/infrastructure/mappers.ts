/**
 * infrastructure/mappers: Backend JSON(snake_case) → domain type(camelCase) conversion.
 *
 * Backend(cxt-backend) wire is snake_case(id, repo_id, doc_hash, created_at …),
 * frontend domain is camelCase(repoId, docHash, createdAt …). This boundary conversion is absorbed.
 * However, CIR(cir field) is a 1:1 schema source of truth (snake_case) and passes through (casting).
 *
 * Dependencies: domain types only.
 */

import type {
  Repo,
  Branch,
  Snapshot,
  Ref,
  SessionDoc,
  MemoryDigest,
  Manifest,
  TeamIdentity,
  ProviderKind,
  FidelityTier,
  RefKind,
  CIRDocument,
} from '../domain/index.js';

export type RawRepo = Record<string, unknown>;
export type RawBranch = Record<string, unknown>;
export type RawSnapshot = Record<string, unknown>;
export type RawRef = Record<string, unknown>;
export type RawSessionDoc = Record<string, unknown>;
export type RawMemoryDigest = Record<string, unknown>;
export type RawManifest = Record<string, unknown>;

// ── raw access helper (safe casting) ──────────────────────────────

function str(raw: Record<string, unknown>, key: string): string {
  const v = raw[key];
  if (typeof v === 'string') return v;
  return v == null ? '' : String(v);
}
function strArr(raw: Record<string, unknown>, key: string): string[] {
  const v = raw[key];
  return Array.isArray(v) ? v.map((x) => String(x)) : [];
}
function num(raw: Record<string, unknown>, key: string): number {
  const v = raw[key];
  return typeof v === 'number' ? v : 0;
}
function objOf(raw: Record<string, unknown>, key: string): Record<string, unknown> {
  const v = raw[key];
  return v != null && typeof v === 'object' ? (v as Record<string, unknown>) : {};
}

function mapIdentity(raw: Record<string, unknown>): TeamIdentity {
  return { name: str(raw, 'name'), email: str(raw, 'email'), team: str(raw, 'team') };
}

// ── mapper ─────────────────────────────────────────────────────

export function mapRepo(raw: RawRepo): Repo {
  return {
    id: str(raw, 'id'),
    remoteUrl: str(raw, 'remote_url'),
    localPath: str(raw, 'local_path'),
    defaultBranch: str(raw, 'default_branch'),
  };
}

export function mapBranch(raw: RawBranch): Branch {
  return { name: str(raw, 'name'), repoId: str(raw, 'repo_id'), head: str(raw, 'head') };
}

export function mapSnapshot(raw: RawSnapshot): Snapshot {
  return {
    id: str(raw, 'id'),
    repoId: str(raw, 'repo_id'),
    branch: str(raw, 'branch'),
    parents: strArr(raw, 'parents'),
    docHash: str(raw, 'doc_hash'),
    provider: str(raw, 'provider') as ProviderKind,
    fidelity: str(raw, 'fidelity') as FidelityTier,
    message: str(raw, 'message'),
    author: mapIdentity(objOf(raw, 'author')),
    createdAt: str(raw, 'created_at'),
  };
}

export function mapRef(raw: RawRef): Ref {
  return {
    kind: str(raw, 'kind') as RefKind,
    name: str(raw, 'name'),
    repoId: str(raw, 'repo_id'),
    target: str(raw, 'target'),
    symbolic: str(raw, 'symbolic'),
  };
}

export function mapSessionDoc(raw: RawSessionDoc): SessionDoc {
  // cir is a 1:1 schema source of truth (snake_case) → pass-through casting.
  return { hash: str(raw, 'hash'), cir: (raw['cir'] ?? {}) as unknown as CIRDocument };
}

export function mapMemoryDigest(raw: RawMemoryDigest): MemoryDigest {
  const fragmentsRaw = raw['fragments'];
  const fragments = Array.isArray(fragmentsRaw)
    ? fragmentsRaw.map((value) => {
        const fragment = value != null && typeof value === 'object' ? (value as Record<string, unknown>) : {};
        return {
          sourceSnapshot: str(fragment, 'source_snapshot'),
          summary: str(fragment, 'summary'),
          keyFacts: strArr(fragment, 'key_facts'),
          openTasks: strArr(fragment, 'open_tasks'),
          tasksAuthoritative: fragment['tasks_authoritative'] === true,
        };
      })
    : undefined;
  const coverageRaw = raw['graft_coverage'];
  const coverage = coverageRaw != null && typeof coverageRaw === 'object'
    ? coverageRaw as Record<string, unknown>
    : undefined;
  const digest: MemoryDigest = {
    snapshotId: str(raw, 'snapshot_id'),
    summary: str(raw, 'summary'),
    keyFacts: strArr(raw, 'key_facts'),
    openTasks: strArr(raw, 'open_tasks'),
    provider: str(raw, 'provider') as ProviderKind,
  };
  if (fragments) digest.fragments = fragments;
  if (coverage) {
    digest.graftCoverage = {
      projectionVersion: num(coverage, 'projection_version'),
      projectionComplete: coverage['projection_complete'] === true,
      lineageFingerprint: str(coverage, 'lineage_fingerprint'),
      graftSeq: num(coverage, 'graft_seq'),
      graftParents: strArr(coverage, 'graft_parents'),
      pinnedSources: strArr(coverage, 'pinned_sources'),
    };
  }
  return digest;
}

export function mapManifest(raw: RawManifest): Manifest {
  const refsRaw = raw['refs'];
  const refs = Array.isArray(refsRaw) ? refsRaw.map((r) => mapRef(r as RawRef)) : [];
  return {
    repoId: str(raw, 'repo_id'),
    refs,
    snapshotIndex: strArr(raw, 'snapshot_index'),
    version: num(raw, 'version'),
    updatedAt: str(raw, 'updated_at'),
  };
}
