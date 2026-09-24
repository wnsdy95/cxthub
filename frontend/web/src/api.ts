import { decodeGraphState, type GraphWire } from './graphWire';
import { validateGraphState } from './graphState';
// Backend cxtd REST client.
//
// Authentication uses HttpOnly session cookies — JS does not store or attach tokens.
// All requests include 'credentials: 'include' to automatically send cookies to the browser.
// Exception: exchangeSession only sends the IDP token in the Authorization header once,
// and the server sets the session cookie in the Set-Cookie response.
import type { StorageUsageReport, RefLogEntry, User, PublicUser, Repository, PublicRepository, RepositoryPatch, Membership, Invite, Repo, Ref, Snapshot, SessionDoc, MemoryDigest, SettingsUpload, DiffEntry, SearchHit, Pending, Unsync, Organization, PublicOrganization, OrganizationMembership, OrganizationPolicy, OrganizationAuditEvent, BreakGlassGrant, OrganizationRole } from './types';
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
  githubOverview: () => call<import('./github').GitHubOverview>('GET', '/github/connections'),
  githubStart: (namespace_id = '', installation_id = 0) => call<{ url: string }>('POST', '/github/requests', { namespace_id, installation_id }),
  githubEdit: (namespace: string, edit: import('./github').GitHubEdit) => call<{ saved: boolean }>('POST', `/github/connections/${encodeURIComponent(namespace)}`, edit),
  githubEnterprise: (id: string) => call<import('./github').EnterpriseGitHubConnection[]>('GET', `/enterprises/${encodeURIComponent(id)}/github-connections`),

 mcpApplications: () => call<import('./types').MCPApplication[]>('GET', '/me/mcp-applications'),
 revokeMCPApplication: (id:string) => call('DELETE', `/me/mcp-applications/${encodeURIComponent(id)}`),
 accountAudit: () => call<import('./types').AccountAuditEvent[]>('GET', '/me/audit'),
 renameSpace: (kind: 'organization' | 'enterprise', id: string, expected_slug: string, slug: string) => call<{path:string}>('POST', `/${kind}s/${encodeURIComponent(id)}/rename`, {expected_slug,slug}),
 transferRepositoryNamespace: (repository: Repository, destination: string) => call<Repository>('POST', `/repositories/${encodeURIComponent(repository.id)}/transfer-namespace`, {destination,expected_namespace_id:repository.owner_namespace_id,expected_slug:repository.slug}),
 invitationInbox: () => call<import('./types').CollaborationInvitation[]>('GET', '/me/invitations'),
 getCollaborationInvitation: (id: string) => call<import('./types').CollaborationInvitation>('GET', `/invitations/${encodeURIComponent(id)}`),
 listCollaborationInvitations: (kind: 'organization' | 'enterprise', id: string) => call<import('./types').CollaborationInvitation[]>('GET', `/${kind}s/${encodeURIComponent(id)}/invitations`),
 createCollaborationInvitation: (kind: 'organization' | 'enterprise', id: string, recipient: string, role: OrganizationRole) => call<import('./types').CollaborationInvitation>('POST', `/${kind}s/${encodeURIComponent(id)}/invitations`, { recipient, role }),
 actOnCollaborationInvitation: (id: string, action: 'accept' | 'decline' | 'revoke' | 'resend') => call<import('./types').CollaborationInvitation>('POST', `/invitations/${encodeURIComponent(id)}/${action}`, {}),
 checkGitHubSync: (repo: string, cursor: string, signal?: AbortSignal) => call<import('./types').SyncAuditPage>('POST', `/repos/${encodeURIComponent(repo)}/github-sync-check`, {cursor}, undefined, signal),
 memoryPositions: (repo: string, snapshot: string, event: string | undefined, signal?: AbortSignal) => {
  const params = new URLSearchParams({snapshot_id: snapshot});
  if (event) params.set('event_id', event);
  return call<import('./types').MemoryPositions>('GET', `/repos/${encodeURIComponent(repo)}/effective-memory/positions?${params}`, undefined, undefined, signal);
 },
 effectiveMemory: (repo: string, selection: import('./types').EffectiveMemorySelection, cursor: string, signal?: AbortSignal) => {
  const params = new URLSearchParams({snapshot_id: selection.snapshot_id, code_commit: selection.code_commit, cursor, limit: '20'});
  if (selection.memory_hash) params.set('memory_hash', selection.memory_hash);
  if (selection.branch) params.set('branch', selection.branch);
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
  pendingView: async (repo: string) => {
    const view = await call<Omit<import('./types').PendingView, 'graph'> & {graph: GraphWire}>('GET', `/repos/${encodeURIComponent(repo)}/pending-view?graph_encoding=indexed-v2`);
    const graph = decodeGraphState(view.graph);
    validateGraphState(graph, view.revision);
    return {...view, graph};
  },
  notifications: (repository: string, signal?: AbortSignal) => call<import('./types').NotificationJob[]>('GET', `/repositories/${encodeURIComponent(repository)}/notifications`, undefined, undefined, signal),
  retryNotification: (repository: string, id: string) => call('POST', `/repositories/${encodeURIComponent(repository)}/notifications/${encodeURIComponent(id)}/retry`, {}),
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

  listTeams: (organization: string) => call<import('./types').Team[]>('GET', `/organizations/${encodeURIComponent(organization)}/teams`),
  createTeam: (organization: string, name: string, slug: string, description: string) => call<import('./types').Team>('POST', `/organizations/${encodeURIComponent(organization)}/teams`, { name, slug, description }),
  deleteTeam: (organization: string, team: string) => call('DELETE', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}`),
  updateTeam: (organization: string, team: string, expected: Pick<Team, 'name' | 'description'>, profile: Pick<Team, 'name' | 'description'>) => call<Team>('PATCH', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}`, { ...profile, expected }),
  listTeamMembers: (organization: string, team: string) => call<import('./types').TeamMembership[]>('GET', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/members`),
  setTeamMember: (organization: string, team: string, user: string, role: 'member' | 'maintainer') => call('PUT', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/members/${encodeURIComponent(user)}`, { role }),
  removeTeamMember: (organization: string, team: string, user: string) => call('DELETE', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/members/${encodeURIComponent(user)}`),
  listTeamRepositories: (organization: string, team: string) => call<import('./types').TeamRepositoryGrant[]>('GET', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/repositories`),
  setTeamRepository: (organization: string, team: string, repository: string, role: import('./roles').Role) => call('PUT', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/repositories/${encodeURIComponent(repository)}`, { role }),
  removeTeamRepository: (organization: string, team: string, repository: string) => call('DELETE', `/organizations/${encodeURIComponent(organization)}/teams/${encodeURIComponent(team)}/repositories/${encodeURIComponent(repository)}`),
  effectiveOrganizationPolicy: (organization: string) => call<OrganizationPolicy>('GET', `/organizations/${encodeURIComponent(organization)}/effective-policy`),
  listEnterprises: () => call<import('./types').Enterprise[]>('GET', '/enterprises'),
  createEnterprise: (name: string, slug: string) => call<import('./types').Enterprise>('POST', '/enterprises', { name, slug }),
  getEnterprise: (slug: string) => call<import('./types').Enterprise>('GET', `/enterprises/${encodeURIComponent(slug)}`),
  updateEnterprise: (id: string, patch: { name?: string; logo?: string; policy?: import('./types').EnterprisePolicy }) => call<import('./types').Enterprise>('PATCH', `/enterprises/${encodeURIComponent(id)}`, patch),
  listEnterpriseMembers: (id: string) => call<import('./types').EnterpriseMembership[]>('GET', `/enterprises/${encodeURIComponent(id)}/members`),
  setEnterpriseMember: (id: string, user: string, role: 'owner' | 'admin' | 'member') => call('PUT', `/enterprises/${encodeURIComponent(id)}/members/${encodeURIComponent(user)}`, { role }),
  removeEnterpriseMember: (id: string, user: string) => call('DELETE', `/enterprises/${encodeURIComponent(id)}/members/${encodeURIComponent(user)}`),
  listEnterpriseOrganizations: (id: string) => call<Organization[]>('GET', `/enterprises/${encodeURIComponent(id)}/organizations`),
  linkEnterpriseOrganization: (id: string, organization: string) => call('PUT', `/enterprises/${encodeURIComponent(id)}/organizations/${encodeURIComponent(organization)}`, {}),
  unlinkEnterpriseOrganization: (id: string, organization: string) => call('DELETE', `/enterprises/${encodeURIComponent(id)}/organizations/${encodeURIComponent(organization)}`),
  listEnterpriseAudit: (id: string) => call<import('./types').EnterpriseAuditEvent[]>('GET', `/enterprises/${encodeURIComponent(id)}/audit`),
  // Repository · Member · Invite
  listRepositories: () => call<Repository[]>('GET', '/repositories'),
  updateMe: (patch: { username?: string; nickname?: string; load_mode?: string; avatar?: string; locale?: string }) =>
    call<User>('PATCH', '/me', patch),
  createCliToken: () => call<{ token: string; expires_at: string }>('POST', '/me/cli-tokens'),
  listSessions: () =>
    call<{ suffix: string; label?: string; created_at: string; expires_at: string; current: boolean }[] | null>('GET', '/me/sessions'),
  revokeSession: (suffix: string) => call<{ status: string }>('DELETE', `/me/sessions/${encodeURIComponent(suffix)}`),
  publicRepository: (username: string, slug: string) =>
    call<PublicRepository>('GET', `/public/repositories/${encodeURIComponent(username)}/${encodeURIComponent(slug)}`),
  publicUser: (username: string) =>
    call<{ user: PublicUser; repositories: PublicRepository[] }>('GET', `/public/users/${encodeURIComponent(username)}`),
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
  updateMemberRole: (repositoryId: string, userId: string, role: 'owner' | 'member') =>
    call<{ status: string }>('PATCH', `/repositories/${encodeURIComponent(repositoryId)}/members/${encodeURIComponent(userId)}`, { role }),
  removeMember: (repositoryId: string, userId: string) =>
    call<{ status: string }>('DELETE', `/repositories/${encodeURIComponent(repositoryId)}/members/${encodeURIComponent(userId)}`),
  createRepository: (name: string) => call<Repository>('POST', '/repositories', { name }),
  updateRepository: (repositoryId: string, patch: RepositoryPatch) =>
    call<Repository>('PATCH', `/repositories/${encodeURIComponent(repositoryId)}`, patch),
  transferRepository: (repositoryId: string, toUserId: string) =>
    call<Repository>('POST', `/repositories/${encodeURIComponent(repositoryId)}/transfer`, { to_user_id: toUserId }),
  syncVisibility: (repositoryId: string) => call<Repository>('POST', `/repositories/${encodeURIComponent(repositoryId)}/sync-visibility`),
  listMembers: (repositoryId: string) => call<Membership[]>('GET', `/repositories/${encodeURIComponent(repositoryId)}/members`),
  listInvites: (repositoryId: string) => call<Invite[] | null>('GET', `/repositories/${encodeURIComponent(repositoryId)}/invites`),
  revokeInvite: (repositoryId: string, token: string) =>
    call<{ status: string }>('POST', `/repositories/${encodeURIComponent(repositoryId)}/invites/${encodeURIComponent(token)}/revoke`),
  createInvite: (repositoryId: string, email: string, role: string, expiresInDays: number) =>
    call<Invite>('POST', `/repositories/${encodeURIComponent(repositoryId)}/invites`, { email, role, expires_in_days: expiresInDays }),
  acceptInvite: (token: string) => call<Repository>('POST', `/invites/${encodeURIComponent(token)}/accept`),

  // Organization administration. Organization roles manage this plane only; they
  // grant repository-wide authority only to Organization owners.
  listOrganizations: () => call<Organization[]>('GET', '/organizations'),
  publicOrganization: (slug: string) =>
    call<PublicOrganization>('GET', `/public/organizations/${encodeURIComponent(slug)}`),
  getOrganization: (organizationId: string) =>
    call<Organization>('GET', `/organizations/${encodeURIComponent(organizationId)}`),
  createOrganization: (name: string, slug: string) => call<Organization>('POST', '/organizations', { name, slug }),
  updateOrganization: (organizationId: string, patch: { name?: string; logo?: string }) =>
    call<Organization>('PATCH', `/organizations/${encodeURIComponent(organizationId)}`, patch),
  listOrganizationMembers: (organizationId: string) =>
    call<OrganizationMembership[]>('GET', `/organizations/${encodeURIComponent(organizationId)}/members`),
  updateOrganizationMember: (organizationId: string, userId: string, role: OrganizationRole) =>
    call<{ status: string }>(
      'PATCH',
      `/organizations/${encodeURIComponent(organizationId)}/members/${encodeURIComponent(userId)}`,
      { role },
    ),
  removeOrganizationMember: (organizationId: string, userId: string, access: 'revoke' | 'retain') =>
    call<{ status: string }>(
      'DELETE',
      `/organizations/${encodeURIComponent(organizationId)}/members/${encodeURIComponent(userId)}?repository_access=${access}`,
    ),
  getOrganizationPolicy: (organizationId: string) =>
    call<OrganizationPolicy>('GET', `/organizations/${encodeURIComponent(organizationId)}/policy`),
  updateOrganizationPolicy: (organizationId: string, patch: Partial<Omit<OrganizationPolicy, 'organization_id' | 'updated_by' | 'updated_at'>> & { expected_updated_at?: string }) =>
    call<OrganizationPolicy>('PATCH', `/organizations/${encodeURIComponent(organizationId)}/policy`, patch),
  listOrganizationRepositories: (organizationId: string) =>
    call<Repository[]>('GET', `/organizations/${encodeURIComponent(organizationId)}/repositories`),
  createOrganizationRepository: (organizationId: string, name: string) =>
    call<Repository>('POST', `/organizations/${encodeURIComponent(organizationId)}/repositories`, { name }),
  organizationAuditPage: (organizationId: string, cursor = '') =>
    call<{ events: OrganizationAuditEvent[]; next_cursor?: string }>('GET', `/organizations/${encodeURIComponent(organizationId)}/audit/page?limit=100&cursor=${encodeURIComponent(cursor)}`),
  listOrganizationAudit: (organizationId: string) =>
    call<OrganizationAuditEvent[]>('GET', `/organizations/${encodeURIComponent(organizationId)}/audit`),
  createBreakGlassGrant: (organizationId: string, repositoryId: string, reason: string, minutes: number) =>
    call<BreakGlassGrant>('POST', `/organizations/${encodeURIComponent(organizationId)}/break-glass`, {
      repository_id: repositoryId,
      reason,
      minutes,
    }),

  // Session Browser — repo branch/commit log/context body
  listRepos: (repositoryId: string) => call<Repo[]>('GET', `/repos?repository=${encodeURIComponent(repositoryId)}`),
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
  graphState: async (repoId: string, position: string, signal?: AbortSignal) => {
    const wire = await call<GraphWire>('GET', `/repos/${encodeURIComponent(repoId)}/graph-state?${new URLSearchParams({position, graph_encoding: 'indexed-v2'})}`, undefined, undefined, signal);
    const g = decodeGraphState(wire);
    validateGraphState(g, g.revision);
    return g;
  },
  repositoryView: async (repoId: string) => {
    const wire = await call<Omit<import('./types').RepositoryView, 'graph'> & {graph: GraphWire}>('GET', `/repos/${encodeURIComponent(repoId)}/view?graph_encoding=indexed-v2`);
    const view = {...wire, graph: decodeGraphState(wire.graph)};
    validateContextSemantics(view.history, view.semantics);
    validateGraphState(view.graph, view.revision);
    return view;
  },
  reflog: (repoId: string) => call<RefLogEntry[]>('GET', `/repos/${encodeURIComponent(repoId)}/reflog`),
  history: (repoId: string) => call<import('./types').HistoryEvent[]>('GET', `/repos/${encodeURIComponent(repoId)}/history`),
  enableContextProtocol: (repoId: string) => call<{ context_protocol: number }>('POST', `/repos/${encodeURIComponent(repoId)}/context-protocol`, {}),
  // Rebase session fork of the same git branch behind its head (graft + ref move, no rewrite).
  joinPreview: (repoId: string, snapshot: string, branch: string | undefined, signal?: AbortSignal) => {
    const params = new URLSearchParams({snapshot});
    if (branch) params.set("branch", branch);
    return call<import("./types").JoinPreview>("GET", `/repos/${encodeURIComponent(repoId)}/join/preview?${params}`, undefined, undefined, signal);
  },
  joinSnapshot: (repoId: string, body: { branch: string; branch_id: string; snapshot: string; include_descendants: boolean; expected_head: string; plan_revision: string }) =>
    call<{ branch: string; head: string; fork_branch?: string }>('POST', `/repos/${encodeURIComponent(repoId)}/join`, body),
  dismissPending: (repoId: string, sessionId: string) =>
    call<{ status: string }>('POST', `/repos/${encodeURIComponent(repoId)}/pending/${encodeURIComponent(sessionId)}/dismiss`, {}),
};
import type { Team } from './types';
