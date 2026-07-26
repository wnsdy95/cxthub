// Backend cxtd REST client.
//
// Authentication uses HttpOnly session cookies — JS does not store or attach tokens.
// All requests include 'credentials: 'include' to automatically send cookies to the browser.
// Exception: exchangeSession only sends the IDP token in the Authorization header once,
// and the server sets the session cookie in the Set-Cookie response.
import type { RefLogEntry, User, PublicUser, Workspace, PublicWorkspace, WorkspacePatch, Membership, Invite, Repo, Ref, Snapshot, SessionDoc, MemoryDigest, SettingsUpload, DiffEntry, SearchHit, Pending, Unsync } from './types';

// Default is same-origin relative path (/api/v1). Dev uses Vite proxy, prod assumes same-domain deployment.
// To serve from a different origin, use an absolute URL with VITE_API_BASE, but cookies must be same-site.
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1';

export interface SessionResponse {
  user: User;
  expires_at: string;
}

async function call<T>(method: string, path: string, body?: unknown, idpToken?: string): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') headers['X-Cxt-CSRF'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (idpToken) headers['Authorization'] = `Bearer ${idpToken}`; // Only used for exchangeSession
  const res = await fetch(BASE + path, {
    method,
    headers,
    credentials: 'include', // HttpOnly session cookie exchange
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    try {
      const e = (await res.json()) as { error?: { message?: string } };
      if (e?.error?.message) detail = e.error.message;
    } catch {
      /* non-JSON error body */
    }
    throw new Error(detail);
  }
  const text = await res.text();
  return (text ? JSON.parse(text) : null) as T;
}

export const api = {
  // Session — exchangeSession exchanges the IDP token to have the server set the session cookie.
  exchangeSession: (idpToken: string) => call<SessionResponse>('POST', '/auth/session', undefined, idpToken),
  logout: () => call<{ status: string }>('DELETE', '/auth/session'),
  me: () => call<User>('GET', '/me'),
  approveDevice: (code: string) => call<{ status: string }>('POST', '/auth/device/approve', { code }),

  // Workspace · Member · Invite
  listWorkspaces: () => call<Workspace[]>('GET', '/workspaces'),
  updateMe: (patch: { username?: string; nickname?: string; load_mode?: string; avatar?: string; locale?: string }) =>
    call<User>('PATCH', '/me', patch),
  createCliToken: () => call<{ token: string; expires_at: string }>('POST', '/me/cli-tokens'),
  listSessions: () =>
    call<{ suffix: string; label?: string; created_at: string; expires_at: string; current: boolean }[] | null>('GET', '/me/sessions'),
  revokeSession: (suffix: string) => call<{ status: string }>('DELETE', `/me/sessions/${encodeURIComponent(suffix)}`),
  publicWorkspace: (username: string, slug: string) =>
    call<PublicWorkspace>('GET', `/public/workspaces/${encodeURIComponent(username)}/${encodeURIComponent(slug)}`),
  publicUser: (username: string) =>
    call<{ user: PublicUser; workspaces: PublicWorkspace[] }>('GET', `/public/users/${encodeURIComponent(username)}`),
  userContributions: (username: string) =>
    call<{ total: number; days: { date: string; count: number }[] }>(
      'GET',
      `/public/users/${encodeURIComponent(username)}/contributions`,
    ),
  userActivity: (username: string) =>
    call<{ months: import('./types').ActivityMonth[] }>(
      'GET',
      `/public/users/${encodeURIComponent(username)}/activity`,
    ),
  listCliTokens: () =>
    call<{ suffix: string; label?: string; created_at: string; expires_at: string }[] | null>('GET', '/me/cli-tokens'),
  revokeCliToken: (suffix: string) => call<{ status: string }>('DELETE', `/me/cli-tokens/${encodeURIComponent(suffix)}`),
  updateMemberRole: (wsId: string, userId: string, role: 'owner' | 'member') =>
    call<{ status: string }>('PATCH', `/workspaces/${encodeURIComponent(wsId)}/members/${encodeURIComponent(userId)}`, { role }),
  removeMember: (wsId: string, userId: string) =>
    call<{ status: string }>('DELETE', `/workspaces/${encodeURIComponent(wsId)}/members/${encodeURIComponent(userId)}`),
  createWorkspace: (name: string) => call<Workspace>('POST', '/workspaces', { name }),
  updateWorkspace: (wsId: string, patch: WorkspacePatch) =>
    call<Workspace>('PATCH', `/workspaces/${encodeURIComponent(wsId)}`, patch),
  transferWorkspace: (wsId: string, toUserId: string) =>
    call<Workspace>('POST', `/workspaces/${encodeURIComponent(wsId)}/transfer`, { to_user_id: toUserId }),
  syncVisibility: (wsId: string) => call<Workspace>('POST', `/workspaces/${encodeURIComponent(wsId)}/sync-visibility`),
  listMembers: (wsId: string) => call<Membership[]>('GET', `/workspaces/${encodeURIComponent(wsId)}/members`),
  listInvites: (wsId: string) => call<Invite[] | null>('GET', `/workspaces/${encodeURIComponent(wsId)}/invites`),
  revokeInvite: (wsId: string, token: string) =>
    call<{ status: string }>('POST', `/workspaces/${encodeURIComponent(wsId)}/invites/${encodeURIComponent(token)}/revoke`),
  createInvite: (wsId: string, email: string, role: string, expiresInDays: number) =>
    call<Invite>('POST', `/workspaces/${encodeURIComponent(wsId)}/invites`, { email, role, expires_in_days: expiresInDays }),
  acceptInvite: (token: string) => call<Workspace>('POST', `/invites/${encodeURIComponent(token)}/accept`),

  // Session Browser — repo branch/commit log/context body
  listRepos: (workspaceId: string) => call<Repo[]>('GET', `/repos?workspace=${encodeURIComponent(workspaceId)}`),
  listRefs: (repoId: string) => call<Ref[]>('GET', `/repos/${encodeURIComponent(repoId)}/refs`),
  listSnapshots: (repoId: string, branch: string) =>
    call<Snapshot[]>('GET', `/repos/${encodeURIComponent(repoId)}/snapshots?branch=${encodeURIComponent(branch)}`),
  getDoc: (repoId: string, hash: string) =>
    call<SessionDoc>('GET', `/repos/${encodeURIComponent(repoId)}/docs/${encodeURIComponent(hash)}`),
  // Fork/Diff — Server API(sync protocol). Fork requires member (write), diff requires viewer (read).
  fork: (repoId: string, from: string, newBranch: string, author: { name: string; email: string }) =>
    call<{ branch: string; head: string }>('POST', `/repos/${encodeURIComponent(repoId)}/fork`, {
      from,
      new_branch: newBranch,
      author: { ...author, team: '' },
    }),
  diff: (repoId: string, left: string, right: string) =>
    call<{ changes: DiffEntry[] | null }>('POST', `/repos/${encodeURIComponent(repoId)}/diff`, { left, right }),
  // Search — Commit message/author + chat body (server scan, viewer required)
  search: (repoId: string, q: string) =>
    call<{ hits: SearchHit[] | null; truncated: boolean }>(
      'GET',
      `/repos/${encodeURIComponent(repoId)}/search?q=${encodeURIComponent(q)}`,
    ),
  updateAbout: (
    repoId: string,
    about: { description?: string; website?: string; topics?: string[]; default_branch?: string; protect_default?: boolean },
  ) =>
    call<Repo>('PATCH', `/repos/${encodeURIComponent(repoId)}/about`, about),
  getSettings: (repoId: string, kind: 'claude' | 'agents' | 'codex') =>
    call<{ kind: string; files: { path: string; content_b64: string }[]; updated_at: string; updated_by?: string }>(
      'GET',
      `/repos/${encodeURIComponent(repoId)}/settings/${kind}`,
    ),
  putSettings: (repoId: string, kind: 'claude' | 'agents' | 'codex', payload: SettingsUpload) =>
    call<{ kind: string; files: number }>('PUT', `/repos/${encodeURIComponent(repoId)}/settings/${kind}`, payload),
  // Rotate: expect = CAS of envelope read as basis for replacement — if other storage intervenes between GET~PUT, server rejects with 409 rotate_conflict, preventing stale re-encryption from overwriting.
  putSecrets: (repoId: string, envelope: unknown, rotate = false, expect = '') =>
    call<{ status: string }>(
      'PUT',
      `/repos/${encodeURIComponent(repoId)}/secrets${rotate ? `?rotate=true&expect=${encodeURIComponent(expect)}` : ''}`,
      envelope,
    ),
  getSecrets: (repoId: string) => call<import('./secretscrypto').SecretsEnvelope>('GET', `/repos/${encodeURIComponent(repoId)}/secrets`),
  getMemory: (repoId: string, snapshotId: string) =>
    call<MemoryDigest>('GET', `/repos/${encodeURIComponent(repoId)}/memories/${encodeURIComponent(snapshotId)}`),
  listPending: (repoId: string) => call<Pending[]>('GET', `/repos/${encodeURIComponent(repoId)}/pending`),
  listUnsync: (repoId: string) => call<Unsync[]>('GET', `/repos/${encodeURIComponent(repoId)}/unsync`),
  deletePending: (repoId: string, sessionId: string) =>
    call<{ status: string }>('DELETE', `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}`),
  // Empty object body: Server enforces application/json for this POST (CSRF 2nd defense — form submission blocking).
  undismissPending: (repoId: string, sessionId: string) =>
    call<{ status: string }>(
      'POST',
      `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}/undismiss`,
      {},
    ),
  reflog: (repoId: string) => call<RefLogEntry[]>('GET', `/repos/${encodeURIComponent(repoId)}/reflog`),
  // Rebase session fork of the same git branch behind its head (graft + ref move, no rewrite).
  joinSnapshot: (repoId: string, body: { branch: string; snapshot: string; include_descendants?: boolean }) =>
    call<{ branch: string; head: string; fork_branch?: string }>('POST', `/repos/${encodeURIComponent(repoId)}/join`, body),
  dismissPending: (repoId: string, sessionId: string) =>
    call<{ status: string }>('POST', `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}/dismiss`, {}),
};
