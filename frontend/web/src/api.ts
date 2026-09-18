// Backend cxtd REST client.
//
// Authentication uses HttpOnly session cookies — JS does not store or attach tokens.
// All requests include 'credentials: 'include' to automatically send cookies to the browser.
// Exception: exchangeSession only sends the IDP token in the Authorization header once,
// and the server sets the session cookie in the Set-Cookie response.
import type { StorageUsageReport, RefLogEntry, User, PublicUser, Workspace, PublicWorkspace, WorkspacePatch, Membership, Invite, Repo, Ref, Snapshot, SessionDoc, MemoryDigest, SettingsUpload, DiffEntry, SearchHit, Pending, Unsync, Enterprise, PublicEnterprise, EnterpriseMembership, EnterprisePolicy, EnterpriseAuditEvent, BreakGlassGrant, EnterpriseRole } from './types';
import { normalizeActivityResponse } from './activity';
import { validateContextSemantics } from './graphEvidence';

// Default is same-origin relative path (/api/v1). Dev uses Vite proxy, prod assumes same-domain deployment.
// To serve from a different origin, use an absolute URL with VITE_API_BASE, but cookies must be same-site.
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1';

export interface SessionResponse {
  user: User;
  expires_at: string;
}

export interface OAuthConsentRequest {
  id: string;
  client_name: string;
  scope: string;
  resource: string;
  redirect_uri: string;
  expires_at: string;
}

export class ApiError extends Error {
  constructor(message: string, public readonly code: string, public readonly status: number) { super(message); }
}

async function call<T>(method: string, path: string, body?: unknown, idpToken?: string, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') headers['X-Cxt-CSRF'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (idpToken) headers['Authorization'] = `Bearer ${idpToken}`; // Only used for exchangeSession
  const res = await fetch(BASE + path, {
    method,
    signal,
    headers,
    credentials: 'include', // HttpOnly session cookie exchange
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    let code = '';
    try {
      const e = (await res.json()) as { error?: { message?: string; code?: string } };
      code = e?.error?.code ?? '';
      if (e?.error?.message) detail = e.error.message;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(detail, code, res.status);
  }
  const text = await res.text();
  return (text ? JSON.parse(text) : null) as T;
}

export interface DocEventPage {
  hash: string;
  envelope: SessionDoc['cir']['envelope'];
  events: SessionDoc['cir']['events'];
  total: number;
  offset: number;
  next: number;
  inherited: number;
}

export const api = {
 effectiveMemory: (repo: string, selection: import('./types').EffectiveMemorySelection, cursor: string, signal?: AbortSignal) => {
  const params = new URLSearchParams({snapshot_id: selection.snapshot_id, code_commit: selection.code_commit, cursor, limit: '20'});
  if (selection.memory_hash) params.set('memory_hash', selection.memory_hash);
  return call<import('./types').EffectiveMemoryPage>('GET', `/repos/${encodeURIComponent(repo)}/effective-memory?${params}`, undefined, undefined, signal);
 },
 codeApplicability: (repo: string, selection: import('./types').CodeSelection, signal?: AbortSignal) => {
  const params = new URLSearchParams({code_commit: selection.code_commit, source_commit: selection.source_commit});
  if (selection.source_parent) params.set('source_parent', selection.source_parent);
  for (const path of selection.paths) params.append('path', path);
  return call<import('./types').CodeApplicabilityResult>('GET', `/repos/${encodeURIComponent(repo)}/code-applicability?${params}`, undefined, undefined, signal);
 },
 gitScans: (repo: string, cursor: string, signal?: AbortSignal) => call<import('./types').GitScanPage>('GET', `/repos/${encodeURIComponent(repo)}/git-scans?limit=20&cursor=${encodeURIComponent(cursor)}`, undefined, undefined, signal),
 retryGitScan: (repo: string, id: string) => call('POST', `/repos/${encodeURIComponent(repo)}/git-scans/${encodeURIComponent(id)}/retry`, {}),
 gitChanges: (repo: string, cursor: string, signal?: AbortSignal) => call<import('./types').GitChangePage>('GET', `/repos/${encodeURIComponent(repo)}/git-changes?limit=20&cursor=${encodeURIComponent(cursor)}`, undefined, undefined, signal),
 retryGitChange: (repo: string, id: string) => call('POST', `/repos/${encodeURIComponent(repo)}/git-changes/${encodeURIComponent(id)}/retry`, {}),
  repositoryChangesURL: (repo: string) => `${BASE}/repos/${encodeURIComponent(repo)}/changes`,
  repositoryRevision: (repo: string) => call<import('./types').RepositoryRevision>('GET', `/repos/${encodeURIComponent(repo)}/revision`),
  pendingView: (repo: string) => call<import('./types').PendingView>('GET', `/repos/${encodeURIComponent(repo)}/pending-view`),
  notifications: (workspace: string, signal?: AbortSignal) => call<import('./types').NotificationJob[]>('GET', `/workspaces/${encodeURIComponent(workspace)}/notifications`, undefined, undefined, signal),
  retryNotification: (workspace: string, id: string) => call('POST', `/workspaces/${encodeURIComponent(workspace)}/notifications/${encodeURIComponent(id)}/retry`, {}),
  storageUsage: (namespace: string, month: string, signal?: AbortSignal) => call<StorageUsageReport>('GET', `${namespace === 'self' ? '/me' : '/namespaces/' + encodeURIComponent(namespace)}/storage?month=${encodeURIComponent(month)}`, undefined, undefined, signal),
  reconcileStorage: (namespace: string) => call<{ reconciled: boolean }>('POST', `${namespace === 'self' ? '/me' : '/namespaces/' + encodeURIComponent(namespace)}/storage/reconcile`, {}),
  prPromotions: (repoId: string, signal?: AbortSignal) => call<import('./types').PRPromotionJob[]>('GET', `/repos/${encodeURIComponent(repoId)}/prs/promotions`, undefined, undefined, signal),
  retryPRPromotion: (repoId: string, id: string) => call('POST', `/repos/${encodeURIComponent(repoId)}/prs/promotions/${encodeURIComponent(id)}/retry`, {}),
  // Session — exchangeSession exchanges the IDP token to have the server set the session cookie.
  exchangeSession: (idpToken: string) => call<SessionResponse>('POST', '/auth/session', undefined, idpToken),
  logout: () => call<{ status: string }>('DELETE', '/auth/session'),
  me: () => call<User>('GET', '/me'),
  approveDevice: (code: string) => call<{ status: string }>('POST', '/auth/device/approve', { code }),
  getOAuthConsent: (requestId: string) =>
    call<OAuthConsentRequest>('GET', `/oauth/requests/${encodeURIComponent(requestId)}`),
  decideOAuthConsent: (requestId: string, approve: boolean) =>
    call<{ redirect_url: string }>('POST', `/oauth/requests/${encodeURIComponent(requestId)}`, { approve }),

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
  userActivity: async (username: string) =>
    normalizeActivityResponse(
      await call<unknown>('GET', `/public/users/${encodeURIComponent(username)}/activity`),
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

  // Enterprise administration. Enterprise roles manage this plane only; they
  // never imply access to a Workspace's repository context.
  listEnterprises: () => call<Enterprise[]>('GET', '/enterprises'),
  publicEnterprise: (slug: string) =>
    call<PublicEnterprise>('GET', `/public/enterprises/${encodeURIComponent(slug)}`),
  getEnterprise: (enterpriseId: string) =>
    call<Enterprise>('GET', `/enterprises/${encodeURIComponent(enterpriseId)}`),
  createEnterprise: (name: string, slug: string) => call<Enterprise>('POST', '/enterprises', { name, slug }),
  updateEnterprise: (enterpriseId: string, patch: { name?: string; logo?: string }) =>
    call<Enterprise>('PATCH', `/enterprises/${encodeURIComponent(enterpriseId)}`, patch),
  listEnterpriseMembers: (enterpriseId: string) =>
    call<EnterpriseMembership[]>('GET', `/enterprises/${encodeURIComponent(enterpriseId)}/members`),
  updateEnterpriseMember: (enterpriseId: string, userId: string, role: EnterpriseRole) =>
    call<{ status: string }>(
      'PATCH',
      `/enterprises/${encodeURIComponent(enterpriseId)}/members/${encodeURIComponent(userId)}`,
      { role },
    ),
  removeEnterpriseMember: (enterpriseId: string, userId: string) =>
    call<{ status: string }>(
      'DELETE',
      `/enterprises/${encodeURIComponent(enterpriseId)}/members/${encodeURIComponent(userId)}`,
    ),
  getEnterprisePolicy: (enterpriseId: string) =>
    call<EnterprisePolicy>('GET', `/enterprises/${encodeURIComponent(enterpriseId)}/policy`),
  updateEnterprisePolicy: (enterpriseId: string, patch: Partial<Omit<EnterprisePolicy, 'enterprise_id' | 'updated_by' | 'updated_at'>>) =>
    call<EnterprisePolicy>('PATCH', `/enterprises/${encodeURIComponent(enterpriseId)}/policy`, patch),
  listEnterpriseWorkspaces: (enterpriseId: string) =>
    call<Workspace[]>('GET', `/enterprises/${encodeURIComponent(enterpriseId)}/workspaces`),
  createEnterpriseWorkspace: (enterpriseId: string, name: string) =>
    call<Workspace>('POST', `/enterprises/${encodeURIComponent(enterpriseId)}/workspaces`, { name }),
  listEnterpriseAudit: (enterpriseId: string) =>
    call<EnterpriseAuditEvent[]>('GET', `/enterprises/${encodeURIComponent(enterpriseId)}/audit`),
  createBreakGlassGrant: (enterpriseId: string, workspaceId: string, reason: string, minutes: number) =>
    call<BreakGlassGrant>('POST', `/enterprises/${encodeURIComponent(enterpriseId)}/break-glass`, {
      workspace_id: workspaceId,
      reason,
      minutes,
    }),

  // Session Browser — repo branch/commit log/context body
  listRepos: (workspaceId: string) => call<Repo[]>('GET', `/repos?workspace=${encodeURIComponent(workspaceId)}`),
  listRefs: (repoId: string) => call<Ref[]>('GET', `/repos/${encodeURIComponent(repoId)}/refs`),
  listSnapshots: (repoId: string, branch: string) =>
    call<Snapshot[]>('GET', `/repos/${encodeURIComponent(repoId)}/snapshots?branch=${encodeURIComponent(branch)}`),
  getDoc: (repoId: string, hash: string) =>
    call<SessionDoc>('GET', `/repos/${encodeURIComponent(repoId)}/docs/${encodeURIComponent(hash)}`),
  getDocEvents: (repoId: string, hash: string, base: string | undefined, offset: number, signal?: AbortSignal) =>
    call<DocEventPage>('GET', `/repos/${encodeURIComponent(repoId)}/docs/${encodeURIComponent(hash)}/events?offset=${offset}&limit=50${base ? `&base=${encodeURIComponent(base)}` : ''}`, undefined, undefined, signal),
  // Fork/Diff — Server API(sync protocol). Fork requires member (write), diff requires viewer (read).
  fork: (repoId: string, from: string, newBranch: string, author: { name: string; email: string }) =>
    call<{ branch: string; head: string }>('POST', `/repos/${encodeURIComponent(repoId)}/fork`, {
      from,
      new_branch: newBranch,
      author: { ...author, team: '' },
    }),
  diff: (repoId: string, left: string, right: string) =>
    call<{ changes: DiffEntry[] | null }>('POST', `/repos/${encodeURIComponent(repoId)}/diff`, { left, right }),
  // Search — Commit metadata and indexed conversation text (viewer required)
  search: (repoId: string, q: string, signal?: AbortSignal) =>
    call<{ hits: SearchHit[] | null; truncated: boolean }>(
      'GET',
      `/repos/${encodeURIComponent(repoId)}/search?q=${encodeURIComponent(q)}`, undefined, undefined, signal,
    ),
  updateAbout: (
    repoId: string,
    about: { description?: string; website?: string; topics?: string[]; default_branch?: string; protect_default?: boolean },
  ) =>
    call<Repo>('PATCH', `/repos/${encodeURIComponent(repoId)}/about`, about),
  getSettings: (repoId: string, kind: 'claude' | 'agents' | 'codex') =>
    call<{ kind: string; files: { path: string; content_b64: string }[]; updated_at: string; updated_by?: string } | null>(
      'GET',
      `/repos/${encodeURIComponent(repoId)}/settings/${kind}`,
    ),
  putSettings: (repoId: string, kind: 'claude' | 'agents' | 'codex', payload: SettingsUpload) =>
    call<{ kind: string; files: number }>('PUT', `/repos/${encodeURIComponent(repoId)}/settings/${kind}`, payload),
  // The editing revision is captured before editing, never refreshed at save time.
  putSecrets: (repoId: string, envelope: unknown, revision: string, rotate = false, expect = '') =>
    call<{ status: string; revision: string }>(
      'PUT',
      `/repos/${encodeURIComponent(repoId)}/secrets?expected_revision=${encodeURIComponent(revision)}${rotate ? `&rotate=true&expect=${encodeURIComponent(expect)}` : ''}`,
      envelope,
    ),
  getSecrets: (repoId: string) =>
    call<import('./secretscrypto').SecretsEnvelope | null>('GET', `/repos/${encodeURIComponent(repoId)}/secrets`),
  getMemory: (repoId: string, snapshotId: string) =>
    call<MemoryDigest>('GET', `/repos/${encodeURIComponent(repoId)}/memories/${encodeURIComponent(snapshotId)}`),
  getMemoryObject: (repoId: string, hash: string) =>
    call<MemoryDigest>('GET', `/repos/${encodeURIComponent(repoId)}/memory-objects/${encodeURIComponent(hash)}`),
  listPending: (repoId: string) => call<Pending[]>('GET', `/repos/${encodeURIComponent(repoId)}/pending`),
  listUnsync: (repoId: string) => call<Unsync[]>('GET', `/repos/${encodeURIComponent(repoId)}/unsync`),
  // Empty object body: Server enforces application/json for this POST (CSRF 2nd defense — form submission blocking).
  undismissPending: (repoId: string, sessionId: string) =>
    call<{ status: string }>(
      'POST',
      `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}/undismiss`,
      {},
    ),
  repositoryView: async (repoId: string) => {
    const view = await call<import('./types').RepositoryView>('GET', `/repos/${encodeURIComponent(repoId)}/view`);
    validateContextSemantics(view.history, view.semantics);
    return view;
  },
  reflog: (repoId: string) => call<RefLogEntry[]>('GET', `/repos/${encodeURIComponent(repoId)}/reflog`),
  history: (repoId: string) => call<import('./types').HistoryEvent[]>('GET', `/repos/${encodeURIComponent(repoId)}/history`),
  enableContextProtocol: (repoId: string) => call<{ context_protocol: number }>('POST', `/repos/${encodeURIComponent(repoId)}/context-protocol`, {}),
  // Rebase session fork of the same git branch behind its head (graft + ref move, no rewrite).
  joinSnapshot: (repoId: string, body: { branch: string; branch_id?: string; snapshot: string; include_descendants?: boolean }) =>
    call<{ branch: string; head: string; fork_branch?: string }>('POST', `/repos/${encodeURIComponent(repoId)}/join`, body),
  dismissPending: (repoId: string, sessionId: string) =>
    call<{ status: string }>('POST', `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}/dismiss`, {}),
};
