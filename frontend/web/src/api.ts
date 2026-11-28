// Backend cxtd REST client.
//
// Authentication uses HttpOnly session cookies — JS does not store or attach tokens.
// All requests include 'credentials: 'include' to automatically send cookies to the browser.
// Exception: exchangeSession only sends the IDP token in the Authorization header once,
// and the server sets the session cookie in the Set-Cookie response.
import type { StorageUsageReport, User, PublicUser, Repository, PublicRepository, RepositoryPatch, Membership, Invite, Repo, SettingsUpload, Organization, PublicOrganization, OrganizationMembership, OrganizationPolicy, OrganizationAuditEvent, OrganizationRole } from './types';
import type { DocFragmentPage, GitHubRepositoryConnection, PushOriginView } from './types';
import { DOCUMENT_DETAIL_BYTES, DOCUMENT_FRAGMENT_BYTES, DOCUMENT_FRAGMENT_RESPONSE_BYTES, DOCUMENT_PAGE_BYTES, readDocumentResponse, scheduleDocumentRead } from './documentReads';
import { DOCUMENT_VISIBLE_MESSAGES, validateAnchoredEventPage, validateDocFragment, type AnchoredEventPage } from './documentPaging';

// Default is same-origin relative path (/api/v1). Dev uses Vite proxy, prod assumes same-domain deployment.
// To serve from a different origin, use an absolute URL with VITE_API_BASE, but cookies must be same-site.
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1';

interface SessionResponse {
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
  constructor(message: string, public readonly code: string, public readonly status: number, public readonly retryAfterMs?: number) { super(message); }
}

export class ApiTransportError extends Error {}

export function parseRetryAfter(value: string | null, now = Date.now()): number | undefined {
  if (!value?.trim()) return undefined;
  const trimmed = value.trim();
  if (/^\d+$/.test(trimmed)) {
    const delay = Number(trimmed) * 1000;
    return Number.isSafeInteger(delay) ? delay : undefined;
  }
  if (!/^[A-Za-z]{3}, /.test(trimmed)) return undefined;
  const timestamp = Date.parse(trimmed);
  return Number.isFinite(timestamp) ? Math.max(0, timestamp - now) : undefined;
}

async function readTransport<T>(read: () => Promise<T>): Promise<T> {
  try { return await read(); }
  catch (error) {
    if (error instanceof TypeError) throw new ApiTransportError(error.message, { cause: error });
    throw error;
  }
}

async function call<T>(method: string, path: string, body?: unknown, idpToken?: string, signal?: AbortSignal, maxBytes?: number, additionalHeaders?: Record<string, string>): Promise<T> {
  // Compatibility declaration only; cookies continue to carry authentication.
  const headers: Record<string, string> = {'X-Cxt-Doc-Identities': 'cxt-manifest-sha256-v1', ...additionalHeaders};
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') headers['X-Cxt-CSRF'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (idpToken) headers['Authorization'] = `Bearer ${idpToken}`; // Only used for exchangeSession
  const res = await readTransport(() => fetch(BASE + path, {
    method,
    signal,
    headers,
    credentials: 'include', // HttpOnly session cookie exchange
    body: body !== undefined ? JSON.stringify(body) : undefined,
  }));
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    let code = '';
    try {
      const e = (maxBytes === undefined ? await res.json() : JSON.parse(await readDocumentResponse(res, maxBytes))) as { error?: { message?: string; code?: string } };
      code = e?.error?.code ?? '';
      if (e?.error?.message) detail = e.error.message;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(detail, code, res.status, parseRetryAfter(res.headers.get('Retry-After')));
  }
  const text = maxBytes === undefined ? await readTransport(() => res.text()) : await readDocumentResponse(res, maxBytes);
  return (text ? JSON.parse(text) : null) as T;
}

function anchoredSessionPath(repoId: string, provider: string, sessionId: string) {
  return `/repos/${encodeURIComponent(repoId)}/anchored-sessions/${encodeURIComponent(provider)}/${encodeURIComponent(sessionId)}`;
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
  notifications: (repository: string, signal?: AbortSignal) => call<import('./types').NotificationJob[]>('GET', `/repositories/${encodeURIComponent(repository)}/notifications`, undefined, undefined, signal),
  retryNotification: (repository: string, id: string) => call('POST', `/repositories/${encodeURIComponent(repository)}/notifications/${encodeURIComponent(id)}/retry`, {}),
  storageUsage: (namespace: string, month: string, signal?: AbortSignal) => call<StorageUsageReport>('GET', `${namespace === 'self' ? '/me' : '/namespaces/' + encodeURIComponent(namespace)}/storage?month=${encodeURIComponent(month)}`, undefined, undefined, signal),
  reconcileStorage: (namespace: string) => call<{ reconciled: boolean }>('POST', `${namespace === 'self' ? '/me' : '/namespaces/' + encodeURIComponent(namespace)}/storage/reconcile`, {}),
  anchoredSessions: (repoId: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredSessionSummary[]>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-sessions`, undefined, undefined, signal),
  anchoredSession: (repoId: string, provider: string, sessionId: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredSession>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-sessions/${encodeURIComponent(provider)}/${encodeURIComponent(sessionId)}`, undefined, undefined, signal),
  anchoredCommit: (repoId: string, sha: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredCommitSegment[]>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-commits/${encodeURIComponent(sha)}`, undefined, undefined, signal),
  /** The summaries written at a commit that the caller may read. */
  setAnchoredSessionArchived: (repoId: string, provider: string, sessionId: string, archived: boolean) =>
    call<import('./types').AnchoredSessionArchive>('PUT', `/repos/${encodeURIComponent(repoId)}/anchored-sessions/${encodeURIComponent(provider)}/${encodeURIComponent(sessionId)}/archive`, { archived }),
  anchoredCommitSummaries: (repoId: string, sha: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredSummary[]>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-commits/${encodeURIComponent(sha)}/summaries`, undefined, undefined, signal),
  /** Looks for a query in what the sessions tab shows, as far as the caller may read it. */
  anchoredSearch: (repoId: string, query: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredSearchResult>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-search?q=${encodeURIComponent(query)}`, undefined, undefined, signal),
  branchLifetimes: (repoId: string, signal?: AbortSignal) =>
    call<import('./types').BranchLifetime[]>('GET', `/repos/${encodeURIComponent(repoId)}/branch-lifetimes`, undefined, undefined, signal),
  /** A page of a session's conversation, read by the session: the server holds it to what the caller may read. */
  anchoredEvents: (repoId: string, provider: string, sessionId: string, offset: number, signal?: AbortSignal) =>
    scheduleDocumentRead(async () => {
      const params = new URLSearchParams({ offset: String(offset), limit: String(DOCUMENT_VISIBLE_MESSAGES) });
      const page = await call<AnchoredEventPage>('GET', `${anchoredSessionPath(repoId, provider, sessionId)}/events?${params}`, undefined, undefined, signal, DOCUMENT_PAGE_BYTES);
      return validateAnchoredEventPage(page, offset, DOCUMENT_VISIBLE_MESSAGES);
    }, signal),
  /** One event of a session's conversation in full, for a body its page deferred. */
  anchoredEvent: (repoId: string, provider: string, sessionId: string, index: number, signal?: AbortSignal) =>
    scheduleDocumentRead(async () => {
      const page = await call<AnchoredEventPage>('GET', `${anchoredSessionPath(repoId, provider, sessionId)}/events?view=raw&offset=${index}&limit=1`, undefined, undefined, signal, DOCUMENT_DETAIL_BYTES);
      const checked = validateAnchoredEventPage(page, index, 1);
      if (checked.events.length !== 1) throw new Error('Invalid session conversation event');
      return checked.events[0];
    }, signal),
  anchoredEventFragment: (repoId: string, provider: string, sessionId: string, index: number, offset: number, signal?: AbortSignal) =>
    scheduleDocumentRead(async () => {
      const params = new URLSearchParams({ index: String(index), offset: String(offset), budget: String(DOCUMENT_FRAGMENT_BYTES) });
      const page = await call<DocFragmentPage>('GET', `${anchoredSessionPath(repoId, provider, sessionId)}/event-fragments?${params}`, undefined, undefined, signal, DOCUMENT_FRAGMENT_RESPONSE_BYTES);
      return validateDocFragment(page, index, offset);
    }, signal),
  anchoredSessionCalls: (repoId: string, provider: string, sessionId: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredAgentCall[]>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-sessions/${encodeURIComponent(provider)}/${encodeURIComponent(sessionId)}/calls`, undefined, undefined, signal),
  anchoredNotes: (repoId: string, signal?: AbortSignal) =>
    call<import('./types').AnchoredNote[]>('GET', `/repos/${encodeURIComponent(repoId)}/anchored-notes`, undefined, undefined, signal),
  createAnchoredNote: (repoId: string, target: import('./types').AnchoredNoteTarget, body: string) =>
    call<import('./types').AnchoredNote>('POST', `/repos/${encodeURIComponent(repoId)}/anchored-notes`, { target, body }),
  updateAnchoredNote: (repoId: string, noteId: string, body: string) =>
    call<import('./types').AnchoredNote>('PUT', `/repos/${encodeURIComponent(repoId)}/anchored-notes/${encodeURIComponent(noteId)}`, { body }),
  deleteAnchoredNote: (repoId: string, noteId: string) =>
    call<void>('DELETE', `/repos/${encodeURIComponent(repoId)}/anchored-notes/${encodeURIComponent(noteId)}`),
  reportedWorktrees: (repoId: string, signal?: AbortSignal) =>
    call<import('./types').ReportedWorktree[]>('GET', `/repos/${encodeURIComponent(repoId)}/worktrees`, undefined, undefined, signal),
  remoteGit: (repoId: string, before: string, signal?: AbortSignal) =>
    call<import('./types').RemoteGitView>('GET', `/repos/${encodeURIComponent(repoId)}/remote-git${before ? `?before=${encodeURIComponent(before)}` : ''}`, undefined, undefined, signal),
  /** The mark that is another one after anything the Sessions tab shows was stored. */
  viewRevision: (repoId: string, signal?: AbortSignal) =>
    call<{ revision: string }>('GET', `/repos/${encodeURIComponent(repoId)}/revision`, undefined, undefined, signal),
  /** Where the repository's newest pushes came from: a computer where cxt runs, or not. */
  pushOrigins: (repoId: string, signal?: AbortSignal) =>
    call<PushOriginView>('GET', `/repos/${encodeURIComponent(repoId)}/push-origins`, undefined, undefined, signal),
  /** Whether the server reads the repository on GitHub through the GitHub App. */
  githubConnection: (repoId: string, signal?: AbortSignal) =>
    call<GitHubRepositoryConnection>('GET', `/repos/${encodeURIComponent(repoId)}/github-connection`, undefined, undefined, signal),
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
  samlView: (id: string) => call<import('./federation').SAMLView>('GET', `/enterprises/${encodeURIComponent(id)}/saml`),
  samlConfigure: (id: string, input: import('./federation').SAMLInput) => call<import('./federation').SAMLView>('POST', `/enterprises/${encodeURIComponent(id)}/saml`, input),
  samlDisable: (id: string, revision: string) => call<{ status: string }>('POST', `/enterprises/${encodeURIComponent(id)}/saml/disable`, { revision }),
  samlAuthorize: (id: string) => call<{ url: string }>('POST', `/enterprises/${encodeURIComponent(id)}/saml/authorize`, {}),
  samlSigning: (id: string, input: import('./federation').SAMLSigningInput) => call<import('./federation').SAMLView>('POST', `/enterprises/${encodeURIComponent(id)}/saml/signing`, input),
  credentialAssurances: (id: string) => call<import('./federation').CredentialAssurancesView>('GET', `/enterprises/${encodeURIComponent(id)}/credential-assurances`),
  credentialAssessment: (id: string) => call<import('./federation').CredentialAssessment>('GET', `/enterprises/${encodeURIComponent(id)}/credential-assessment`),
  ownerRecovery: (id: string) => call<import('./federation').OwnerRecoveryView>('GET', `/enterprises/${encodeURIComponent(id)}/owner-recovery`),
  prepareOwnerRecovery: (id: string, revision: string) => call<import('./federation').PreparedOwnerRecovery>('POST', `/enterprises/${encodeURIComponent(id)}/owner-recovery/prepare`, { revision }),
  confirmOwnerRecovery: (id: string, revision: string, code: string) => call('POST', `/enterprises/${encodeURIComponent(id)}/owner-recovery/confirm`, { revision, code }),
  revokeOwnerRecovery: (id: string, revision: string) => call('POST', `/enterprises/${encodeURIComponent(id)}/owner-recovery/revoke`, { revision }),
  redeemOwnerRecovery: (id: string, code: string) => call<import('./federation').OwnerRecoveryView>('POST', `/enterprises/${encodeURIComponent(id)}/owner-recovery/redeem`, { code }),
  assurancePolicy: (id: string, input: { revision: string; max_age_hours: number }) => call('POST', `/enterprises/${encodeURIComponent(id)}/assurance-policy`, input),
  approveCredential: (id: string, target: string, input: import('./federation').CredentialApprovalInput) => call('POST', `/enterprises/${encodeURIComponent(id)}/credential-assurances/${encodeURIComponent(target)}`, input),
  revokeCredentialAssurance: (id: string, target: string) => call('POST', `/enterprises/${encodeURIComponent(id)}/credential-assurances/${encodeURIComponent(target)}/revoke`, {}),
  oidcView: (id: string) => call<import('./federation').OIDCView>('GET', `/enterprises/${encodeURIComponent(id)}/oidc`),
  oidcConfigure: (id: string, input: import('./federation').OIDCInput) => call<import('./federation').OIDCView>('POST', `/enterprises/${encodeURIComponent(id)}/oidc`, input),
  oidcDisable: (id: string, revision: string) => call<{ status: string }>('POST', `/enterprises/${encodeURIComponent(id)}/oidc/disable`, { revision }),
  oidcAuthorize: (id: string) => call<{ url: string }>('POST', `/enterprises/${encodeURIComponent(id)}/oidc/authorize`, {}),
  enterpriseDomains: (id: string) => call<import('./enterpriseDomains').EnterpriseDomain[]>('GET', `/enterprises/${encodeURIComponent(id)}/domains`),
  enterpriseDomainCommand: (id: string, action: 'request' | 'verify' | 'release', domain: string, revision: string) => call<import('./enterpriseDomains').EnterpriseDomain | { status: string }>('POST', `/enterprises/${encodeURIComponent(id)}/domains${action === 'request' ? '' : `/${action}`}`, { domain, revision }),
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
  updateMe: (patch: { username?: string; nickname?: string; avatar?: string; locale?: string }) =>
    call<User>('PATCH', '/me', patch),
  createCliToken: () => call<{ token: string; expires_at: string }>('POST', '/me/cli-tokens'),
  listSessions: () =>
    call<{ suffix: string; label?: string; created_at: string; expires_at: string; current: boolean }[] | null>('GET', '/me/sessions'),
  revokeSession: (suffix: string) => call<{ status: string }>('DELETE', `/me/sessions/${encodeURIComponent(suffix)}`),
  publicRepository: (username: string, slug: string) =>
    call<PublicRepository>('GET', `/public/repositories/${encodeURIComponent(username)}/${encodeURIComponent(slug)}`),
  publicUser: (username: string) =>
    call<{ user: PublicUser; repositories: PublicRepository[] }>('GET', `/public/users/${encodeURIComponent(username)}`),
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

  // The Git remotes connected to a repository, their About fields, team settings and secrets.
  listRepos: (repositoryId: string) => call<Repo[]>('GET', `/repos?repository=${encodeURIComponent(repositoryId)}`),
  updateAbout: (
    repoId: string,
    about: { description?: string; website?: string; topics?: string[] },
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
};
import type { Team } from './types';
