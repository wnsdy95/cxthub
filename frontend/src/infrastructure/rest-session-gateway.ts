/**
 * infrastructure/rest-session-gateway: SessionGateway outbound port REST implementation.
 *
 * Implements the SessionGateway interface from application/ports/session-gateway.ts
 * using HTTP REST API (sync protocol) from backend cxt serve.
 *
 * Responsibilities:
 *   - Calls backend using HttpClient.
 *   - Converts response raw JSON to domain types using mappers.ts.
 *   - Combines paths using ApiRoutes constants.
 *
 * Dependencies: HttpClient, ApiRoutes, mappers, application port interfaces, domain types.
 * (presentation does not directly import this class — only port interfaces are referenced)
 */

import type { SessionGateway } from '../application/ports/session-gateway.js';
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
} from '../application/dto.js';
import type {
  Repo,
  Branch,
  Snapshot,
  Ref,
  SessionDoc,
  MemoryDigest,
  Manifest,
} from '../domain/index.js';
import { HttpClient } from './http/http-client.js';
import { ApiRoutes } from './http/api-routes.js';
import {
  mapRepo,
  mapBranch,
  mapSnapshot,
  mapSessionDoc,
  mapMemoryDigest,
  mapManifest,
  mapRef,
} from './mappers.js';

/**
 * SessionGateway fetch-based REST implementation.
 * Created and injected only in the composition root (presentation/bootstrap/container.ts).
 */
type Raw = Record<string, unknown>;

export class RestSessionGateway implements SessionGateway {
  constructor(private readonly http: HttpClient) {}

  // ── Retrieval (Read) ──────────────────────────────────────────────

  async listRepos(): Promise<Repo[]> {
    const raw = await this.http.get<Raw[] | null>(ApiRoutes.repos());
    return (raw ?? []).map(mapRepo);
  }

  // Backend /branches returns a ref list → selects Branch only where kind=branch (B1).
  async listBranches(repoId: string): Promise<Branch[]> {
    const raw = await this.http.get<Raw[] | null>(ApiRoutes.branches(repoId));
    return (raw ?? [])
      .map(mapRef)
      .filter((r) => r.kind === 'branch')
      .map((r) => ({ name: r.name, repoId: r.repoId, head: r.target }));
  }

  async listSessions(input: ListInput): Promise<ListOutput> {
    const q = input.branch ? `?branch=${encodeURIComponent(input.branch)}` : '';
    const snapsRaw = await this.http.get<Raw[] | null>(ApiRoutes.snapshots(input.repoId) + q);
    const refsRaw = await this.http.get<Raw[] | null>(ApiRoutes.refs(input.repoId));
    return {
      snapshots: (snapsRaw ?? []).map(mapSnapshot),
      refs: (refsRaw ?? []).map(mapRef),
    };
  }

  async getSnapshot(repoId: string, id: string): Promise<Snapshot> {
    return mapSnapshot(await this.http.get<Raw>(ApiRoutes.snapshot(repoId, id)));
  }

  async getDoc(repoId: string, docHash: string): Promise<SessionDoc> {
    return mapSessionDoc(await this.http.get<Raw>(ApiRoutes.doc(repoId, docHash)));
  }

  async getManifest(repoId: string): Promise<Manifest> {
    return mapManifest(await this.http.get<Raw>(ApiRoutes.manifest(repoId)));
  }

  async getMemory(repoId: string, snapshotId: string): Promise<MemoryDigest> {
    return mapMemoryDigest(await this.http.get<Raw>(ApiRoutes.memory(repoId, snapshotId)));
  }

  // ── Changes/Actions ────────────────────────────────────────────────
  // diff/fork/load action endpoints are currently unimplemented (501) in backend (next slice).
  // Gateway calls contractually, and server implementation only requires response mapping enhancement.

  async diff(input: DiffInput): Promise<DiffOutput> {
    return this.http.post<DiffOutput>(ApiRoutes.diff(input.repoId), {
      left: input.left,
      right: input.right,
    });
  }

  async fork(input: ForkInput): Promise<ForkOutput> {
    return this.http.post<ForkOutput>(ApiRoutes.fork(input.repoId), {
      from: input.fromSnapshot,
      new_branch: input.newBranch,
      author: input.author,
    });
  }

  // load action composes and resumes session files on user local machine, which server cannot do.
  // In web, it provides guidance, while actual restoration is performed by CLI (`cxt checkout/load`).
  load(_input: LoadInput): Promise<LoadOutput> {
    throw new Error(
      'load is a local operation — run `cxt checkout <ref>` (or `cxt load`) on your machine',
    );
  }

  // checkout(=fork+load): Server-side fork (branch creation) is possible with fork(), but the load step is local. Web checkout is guided via CLI for fork branch creation only.
  checkout(_input: CheckoutInput): Promise<CheckoutOutput> {
    throw new Error(
      'checkout restores into your local machine — run `cxt checkout -b <branch> --from <ref>` on your machine (use fork() to create a server-side branch)',
    );
  }

  memorize(_input: MemorizeInput): Promise<MemorizeOutput> {
    // memorize is an empirical verification of the active session (local file) distillation, not possible on the web — `cxt memorize` CLI exclusive.
    throw new Error('memorize is CLI-only and not available via the web gateway');
  }

  syncPush(_input: SyncInput): Promise<SyncOutput> {
    throw new Error('sync is a CLI operation — use `cxt push`');
  }

  syncPull(_input: SyncInput): Promise<SyncOutput> {
    throw new Error('sync is a CLI operation — use `cxt pull`');
  }
}
