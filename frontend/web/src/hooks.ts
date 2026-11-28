// React Query — Server State (Query/Mutation).
//
// Authentication status is represented by the `me` query: success (200) → logged in, failure (401) → logged out.
// Tokens are stored in HttpOnly cookies, which JS cannot read, so cookie validity is determined by the single judge, the `me` query on the server.
import { useEffect, useRef } from 'react';
import { useQuery, useInfiniteQuery, useMutation, useQueryClient, keepPreviousData } from '@tanstack/react-query';
import { api } from './api';
import { documentReadRetry } from './readRetry';
import type { User } from './types';
import { firebaseEnabled, devIdpToken, firebaseEmailIdToken, firebaseEmailSignUp, firebaseSocialIdToken, firebaseVerification, firebaseSignOut } from './auth';
import { useT } from './i18n';
import { useDocumentReadScope } from './documentReadScope';
import { useDocumentEventReader } from './documentEventReader';
import { documentBodyKey } from './documentPageCache';
import { followRevision, SESSIONS_FALLBACK_MS, VIEW_REVISION_MS, type SeenRevision } from './viewRevision';

// ── Authentication/Query ─────────────────────────────────────────
// me: Called once on boot to check cookie session validity. A 401 is normal (not logged in), so no retry.
// retryOnMount:false is crucial — without it, components subscribing to an errored `me` query would re-query on mount, causing status='pending' (no data) and me.isLoading to become true. This would cause an infinite loop of /me unmounting and remounting in the App's loading gate.
export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: api.me, retry: false, retryOnMount: false, staleTime: Infinity });
}
function useAuthed() {
  return Boolean(useMe().data);
}
export function useRepositories(enabled = true) {
  const authed = useAuthed();
  return useQuery({ queryKey: ['repositories'], queryFn: api.listRepositories, enabled: authed && enabled });
}
export function useOrganizations() {
  const authed = useAuthed();
  return useQuery({ queryKey: ['organizations'], queryFn: api.listOrganizations, enabled: authed });
}
export function useCreateOrganization() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { name: string; slug: string }) => api.createOrganization(v.name, v.slug),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['organizations'] }),
  });
}
export function useUpdateOrganization() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; patch: { name?: string; logo?: string } }) =>
      api.updateOrganization(v.organizationId, v.patch),
    onSuccess: (organization) => {
      qc.setQueryData(['organization', organization.id], organization);
      void qc.invalidateQueries({ queryKey: ['organizations'] });
      void qc.invalidateQueries({ queryKey: ['publicOrganization', organization.slug] });
    },
  });
}
export function useOrganizationMembers(organizationId: string | null) {
  return useQuery({
    queryKey: ['organization-members', organizationId],
    queryFn: () => api.listOrganizationMembers(organizationId as string),
    enabled: Boolean(organizationId),
  });
}
export function useUpdateOrganizationMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; userId: string; role: import('./types').OrganizationRole }) =>
      api.updateOrganizationMember(v.organizationId, v.userId, v.role),
    onSuccess: (_result, v) => {
      void qc.invalidateQueries({ queryKey: ['organization-members', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['organization-repositories', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['repositories'] });
      void qc.invalidateQueries({ queryKey: ['organizations'] });
    },
  });
}
export function useRemoveOrganizationMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; userId: string; access: 'revoke' | 'retain' }) => api.removeOrganizationMember(v.organizationId, v.userId, v.access),
    onSuccess: (_result, v) => {
      void qc.invalidateQueries({ queryKey: ['organization-members', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['teams', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['organization-repositories', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['teamMembers', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['repositories'] });
      void qc.invalidateQueries({ queryKey: ['organizations'] });
    },
  });
}
export function useOrganizationPolicy(organizationId: string | null) {
  return useQuery({
    queryKey: ['organization-policy', organizationId],
    queryFn: () => api.getOrganizationPolicy(organizationId as string),
    enabled: Boolean(organizationId),
  });
}
export function useUpdateOrganizationPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: {
      organizationId: string;
      patch: Partial<Omit<import('./types').OrganizationPolicy, 'organization_id' | 'updated_by' | 'updated_at'>> & { expected_updated_at?: string };
    }) => api.updateOrganizationPolicy(v.organizationId, v.patch),
    onSuccess: async (policy) => {
      qc.setQueryData(['organization-policy', policy.organization_id], policy);
      await Promise.all([
        qc.invalidateQueries({ queryKey: ['repositories'] }),
        qc.invalidateQueries({ queryKey: ['organization-repositories', policy.organization_id] }),
      ]);
    },
  });
}
export function useOrganizationRepositories(organizationId: string | null) {
  return useQuery({
    queryKey: ['organization-repositories', organizationId],
    queryFn: () => api.listOrganizationRepositories(organizationId as string),
    enabled: Boolean(organizationId),
  });
}
export function useCreateOrganizationRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; name: string }) => api.createOrganizationRepository(v.organizationId, v.name),
    onSuccess: (_repository, v) => {
      void qc.invalidateQueries({ queryKey: ['organization-repositories', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['repositories'] });
    },
  });
}
// Account settings: nickname is lightweight, while username changes URLs and repository paths.
export function useUpdateMe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: { username?: string; nickname?: string; avatar?: string; locale?: string }) =>
      api.updateMe(patch),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u);
      void qc.invalidateQueries({ queryKey: ['publicUser'] });
      qc.invalidateQueries({ queryKey: ['repositories'] }); // owner_username denormalization reflected
    },
  });
}
// Repository settings (public scope · permission policy — owner exclusive, partial PATCH).
export function useUpdateRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; patch: import('./types').RepositoryPatch }) => api.updateRepository(v.repositoryId, v.patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repositories'] }),
  });
}
// Creator or Organization Owner transfer; refresh server-projected permissions.
export function useTransferRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; toUserId: string }) => api.transferRepository(v.repositoryId, v.toUserId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repositories'] }),
  });
}
// GitHub public state manual sync (owner only).
export function useSyncVisibility() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (repositoryId: string) => api.syncVisibility(repositoryId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repositories'] }),
  });
}
// CLI token: issue (expose once — cxt login <token>) · list · revoke.
export function useCreateCliToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createCliToken,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['cli-tokens'] }),
  });
}
export function useCliTokens(enabled: boolean) {
  return useQuery({ queryKey: ['cli-tokens'], queryFn: api.listCliTokens, enabled });
}
// Device session list · revoke — invalidating the current session will make me invalid, logging out.
export function useWebSessions(enabled: boolean) {
  return useQuery({ queryKey: ['web-sessions'], queryFn: api.listSessions, enabled });
}
export function useRevokeWebSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (suffix: string) => api.revokeSession(suffix),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['web-sessions'] });
      qc.invalidateQueries({ queryKey: ['me'] }); // Invalidate current session on logout.
    },
  });
}
export function useRevokeCliToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (suffix: string) => api.revokeCliToken(suffix),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['cli-tokens'] }),
  });
}
// Member management (role change · removal — owner only, allow self-exit).
export function useUpdateMemberRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; userId: string; role: 'owner' | 'member' }) =>
      api.updateMemberRole(v.repositoryId, v.userId, v.role),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['members', v.repositoryId] }),
  });
}
export function useRemoveMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; userId: string }) => api.removeMember(v.repositoryId, v.userId),
    onSuccess: (_r, v) => {
      qc.invalidateQueries({ queryKey: ['members', v.repositoryId] });
      qc.invalidateQueries({ queryKey: ['repositories'] }); // Reflect self-exit.
    },
  });
}
export function useMembers(repositoryId: string | null) {
  return useQuery({
    queryKey: ['members', repositoryId],
    queryFn: () => api.listMembers(repositoryId as string),
    enabled: Boolean(repositoryId),
  });
}
export function useRepos(repositoryId: string | null) {
  const authed = useAuthed();
  return useQuery({
    queryKey: ['repos', repositoryId],
    queryFn: () => api.listRepos(repositoryId as string),
    enabled: authed && Boolean(repositoryId),
  });
}
export function useDocEvent(repoId: string, hash: string, index: number) {
  const owner = useDocumentReadScope();
  const reader = useDocumentEventReader();
  return useQuery({
    queryKey: owner ? documentBodyKey(owner, 'event', repoId, hash, index) : ['doc-event-detail', repoId, hash, index],
    queryFn: ({ signal }) => reader.event(index, signal),
    enabled: query => Boolean(repoId && hash) && query.state.status !== 'error',
    staleTime: Infinity,
    gcTime: owner ? Infinity : 0,
    meta: { documentBody: Boolean(owner) },
    ...documentReadRetry,
    retryOnMount: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

export function useDocEventFragment(repoId: string, hash: string, index: number, offset: number) {
  const owner = useDocumentReadScope();
  const reader = useDocumentEventReader();
  return useQuery({
    queryKey: owner ? documentBodyKey(owner, 'fragment', repoId, hash, index, offset) : ['doc-event-fragment', repoId, hash, index, offset],
    queryFn: ({ signal }) => reader.fragment(index, offset, signal),
    enabled: query => Boolean(repoId && hash) && query.state.status !== 'error',
    staleTime: Infinity,
    gcTime: owner ? Infinity : 0,
    meta: { documentBody: Boolean(owner) },
    ...documentReadRetry,
    retryOnMount: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

// ── Mutation ──────────────────────────────────────────
export function useCreateRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.createRepository(name),
    onSuccess: (repository) => Promise.all([
      qc.invalidateQueries({ queryKey: ['repositories'] }),
      qc.invalidateQueries({ queryKey: ['publicUser', repository.owner_username] }),
    ]),
  });
}
export function useCreateInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; role?: string; email?: string; expiresInDays?: number }) =>
      api.createInvite(v.repositoryId, v.email ?? '', v.role ?? 'member', v.expiresInDays ?? 0),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['invites', v.repositoryId] }),
  });
}
/**
 * Keeps the Sessions tab current while it is in view: asks for the
 * repository's mark every few seconds and reads everything the tab shows
 * again when the mark is another one. Nothing is read again while the mark
 * stays the same, so an open tab costs one short request every few seconds.
 */
export function useFollowViewRevision(repoId: string | null) {
  const qc = useQueryClient();
  const seen = useRef<SeenRevision | null>(null);
  const revision = useQuery({
    queryKey: ['view-revision', repoId],
    queryFn: ({ signal }) => api.viewRevision(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: VIEW_REVISION_MS,
  }).data?.revision;
  useEffect(() => {
    if (!repoId || !revision) return;
    const next = followRevision(seen.current, repoId, revision);
    seen.current = next.seen;
    for (const key of next.readAgain) void qc.invalidateQueries({ queryKey: [key, repoId] });
  }, [qc, repoId, revision]);
}

// Anchored sessions — the caller's own reports. The lists are read again when
// the repository's mark moved (useFollowViewRevision), on request, and as a
// fallback every SESSIONS_FALLBACK_MS.
export function useAnchoredSessions(repoId: string | null) {
  return useQuery({
    queryKey: ['anchored-sessions', repoId],
    queryFn: ({ signal }) => api.anchoredSessions(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
export function useBranchLifetimes(repoId: string | null) {
  return useQuery({
    queryKey: ['branch-lifetimes', repoId],
    queryFn: ({ signal }) => api.branchLifetimes(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
/** One report in full. `revision` is the report's revision in the list, so a report that was replaced is read again. */
export function useAnchoredSession(repoId: string | null, provider: string | null, sessionId: string | null, revision = 0) {
  return useQuery({
    queryKey: ['anchored-session', repoId, provider, sessionId, revision],
    queryFn: ({ signal }) => api.anchoredSession(repoId as string, provider as string, sessionId as string, signal),
    enabled: Boolean(repoId && provider && sessionId),
    retry: false,
    placeholderData: keepPreviousData,
  });
}
/**
 * A session's conversation from `start`, read by the session. What another
 * member reads can have gaps where parts are not open: a page ends at a gap,
 * and reading goes on at the next open part, among `starts`.
 */
export function useAnchoredEvents(repoId: string, provider: string, sessionId: string, revision: number, start: number, starts: number[], enabled: boolean) {
  return useInfiniteQuery({
    queryKey: ['anchored-events', repoId, provider, sessionId, revision, start],
    queryFn: ({ pageParam, signal }) => api.anchoredEvents(repoId, provider, sessionId, pageParam, signal),
    initialPageParam: start,
    getNextPageParam: page => (page.next !== -1 ? page.next : starts.find(next => next >= page.offset + Math.max(page.events.length, 1))),
    enabled,
    ...documentReadRetry,
    staleTime: Infinity,
  });
}
/** The segments that end at a commit. `revisions` names the reports linked to it as the list shows them, so a replaced report is read again. */
export function useAnchoredCommit(repoId: string | null, sha: string | null, revisions = '') {
  return useQuery({
    queryKey: ['anchored-commit', repoId, sha, revisions],
    queryFn: ({ signal }) => api.anchoredCommit(repoId as string, sha as string, signal),
    enabled: Boolean(repoId && sha),
    retry: false,
    placeholderData: keepPreviousData,
  });
}
/** The summaries written at a commit. They change when a report of a session linked to the commit does. */
export function useAnchoredCommitSummaries(repoId: string | null, sha: string | null, revisions = '') {
  return useQuery({
    queryKey: ['anchored-commit-summaries', repoId, sha, revisions],
    queryFn: ({ signal }) => api.anchoredCommitSummaries(repoId as string, sha as string, signal),
    enabled: Boolean(repoId && sha),
    retry: false,
    placeholderData: keepPreviousData,
  });
}
/** What a search of the sessions tab found. A query shorter than two characters asks for nothing. */
export function useAnchoredSearch(repoId: string | null, query: string) {
  const asked = query.trim();
  return useQuery({
    queryKey: ['anchored-search', repoId, asked],
    queryFn: ({ signal }) => api.anchoredSearch(repoId as string, asked, signal),
    enabled: Boolean(repoId) && asked.length >= 2,
    retry: false,
  });
}
/** The recorded agent calls that name a session as sender or receiver. */
export function useAnchoredSessionCalls(repoId: string | null, provider: string | null, sessionId: string | null, revisions = '') {
  return useQuery({
    queryKey: ['anchored-session-calls', repoId, provider, sessionId, revisions],
    queryFn: ({ signal }) => api.anchoredSessionCalls(repoId as string, provider as string, sessionId as string, signal),
    enabled: Boolean(repoId && provider && sessionId),
    retry: false,
    placeholderData: keepPreviousData,
  });
}
export function useReportedWorktrees(repoId: string | null) {
  return useQuery({
    queryKey: ['reported-worktrees', repoId],
    queryFn: ({ signal }) => api.reportedWorktrees(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
// Manual notes — the caller's own. A write stores the note the server
// answered with, because the server masks the text before it keeps it.
export function useAnchoredNotes(repoId: string | null) {
  return useQuery({
    queryKey: ['anchored-notes', repoId],
    queryFn: ({ signal }) => api.anchoredNotes(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
  });
}
/** Archives a session or restores it. Everything the tab lists is read again: an archived session leaves all of it. */
export function useSetAnchoredSessionArchived() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (value: { repoId: string; provider: string; sessionId: string; archived: boolean }) =>
      api.setAnchoredSessionArchived(value.repoId, value.provider, value.sessionId, value.archived),
    onSuccess: (_result, value) => Promise.all(['anchored-sessions', 'anchored-session', 'anchored-commit', 'anchored-commit-summaries', 'anchored-search']
      .map(key => qc.invalidateQueries({ queryKey: [key, value.repoId] }))),
  });
}
export function useSaveAnchoredNote() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (value: { repoId: string; noteId?: string; target: import('./types').AnchoredNoteTarget; body: string }) =>
      value.noteId ? api.updateAnchoredNote(value.repoId, value.noteId, value.body) : api.createAnchoredNote(value.repoId, value.target, value.body),
    onSuccess: (_note, value) => qc.invalidateQueries({ queryKey: ['anchored-notes', value.repoId] }),
  });
}
export function useDeleteAnchoredNote() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (value: { repoId: string; noteId: string }) => api.deleteAnchoredNote(value.repoId, value.noteId),
    onSuccess: (_result, value) => qc.invalidateQueries({ queryKey: ['anchored-notes', value.repoId] }),
  });
}
/** Commits, branches and pull requests as the server read them from the hosting provider, newest commits first. */
// Where the pushes came from. A server without the route leaves the tab as it was: commits without a label.
export function usePushOrigins(repoId: string | null) {
  return useQuery({
    queryKey: ['push-origins', repoId],
    queryFn: ({ signal }) => api.pushOrigins(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
// Whether the server reads the repository on GitHub through the GitHub App. Asked for a member only: with null
// nothing is asked. A server without the route leaves the tab as it was.
export function useGitHubConnection(repoId: string | null) {
  return useQuery({
    queryKey: ['github-connection', repoId],
    queryFn: ({ signal }) => api.githubConnection(repoId as string, signal),
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
export function useRemoteGit(repoId: string | null) {
  return useInfiniteQuery({
    queryKey: ['remote-git', repoId],
    queryFn: ({ pageParam, signal }) => api.remoteGit(repoId as string, pageParam, signal),
    initialPageParam: '',
    getNextPageParam: page => page.next || undefined,
    enabled: Boolean(repoId),
    retry: false,
    refetchInterval: SESSIONS_FALLBACK_MS,
  });
}
// Invite list/redemption — only maintainers can view (403 is disabled).
export function useInvites(repositoryId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['invites', repositoryId],
    queryFn: () => api.listInvites(repositoryId as string),
    enabled: enabled && Boolean(repositoryId),
    retry: false,
  });
}
export function useRevokeInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repositoryId: string; token: string }) => api.revokeInvite(v.repositoryId, v.token),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['invites', v.repositoryId] }),
  });
}
export function useAcceptInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (token: string) => api.acceptInvite(token),
    onSuccess: async () => {
      await qc.cancelQueries({ queryKey: ['repositories'] });
      await qc.invalidateQueries({ queryKey: ['repositories'] });
    },
  });
}

export function useUpdateAbout() {
  const qc = useQueryClient();
  return useMutation({
    // Real PATCH — only update passed fields (fields without a server keep their original values).
    mutationFn: (v: {
      repoId: string;
      description?: string;
      website?: string;
      topics?: string[];
    }) =>
      api.updateAbout(v.repoId, {
        description: v.description,
        website: v.website,
        topics: v.topics,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repos'] }),
  });
}
export function useSettingsBundle(repoId: string | null, kind: 'claude' | 'agents' | 'codex', enabled: boolean) {
  return useQuery({
    queryKey: ['settings', repoId, kind],
    queryFn: () => api.getSettings(repoId as string, kind),
    enabled: enabled && Boolean(repoId),
    retry: false, // 204/null = unset (normal)
  });
}
// Secret envelope metadata (ciphertext — server storage status and update timestamp. Decryption is only by user passphrase).
export function useSecretsEnvelope(repoId: string | null) {
  return useQuery({
    queryKey: ['secrets', repoId],
    queryFn: () => api.getSecrets(repoId as string),
    enabled: Boolean(repoId),
    retry: false, // 204/null = unset (normal)
  });
}
export function usePutSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; kind: 'claude' | 'agents' | 'codex'; files: { path: string; content_b64: string }[] }) =>
      api.putSettings(v.repoId, v.kind, { files: v.files }),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['settings', v.repoId, v.kind] }),
  });
}

// ── Login/Logout ───────────────────────────────────
type LoginInput =
  | { mode: 'dev'; email: string; name: string }
  | { mode: 'email'; email: string; password: string }
  | { mode: 'google' }
  | { mode: 'github' }
  | { mode: 'verification'; action: 'check' | 'resend' | 'email'; email?: string };

export function useLogin() {
  const qc = useQueryClient();
  const t = useT();
  return useMutation({
    mutationFn: async (input: LoginInput) => {
      let idp: string;
      if (input.mode === 'google' || input.mode === 'github') idp = await firebaseSocialIdToken(input.mode, t);
      else if (input.mode === 'verification') idp = await firebaseVerification(input.action, input.email ?? '', t);
      else if (input.mode === 'email') idp = await firebaseEmailIdToken(input.email, input.password, t);
      else idp = devIdpToken(input.email, input.name);
      return api.exchangeSession(idp); // Server sets session cookie via Set-Cookie
    },
    // Immediately fill me cache to transition gate to logged-in state (cookie is already set).
    onSuccess: (res) => qc.setQueryData<User>(['me'], res.user),
  });
}

// useSignUp — Email sign-up. Creates account + sends authentication email only; no session exchange.
// (Login required after clicking authentication link to issue session). Success UI is handled by caller (Login).
export function useSignUp() {
  const t = useT();
  return useMutation({
    mutationFn: (input: { email: string; password: string }) => firebaseEmailSignUp(input.email, input.password, t),
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      try {
        await api.logout(); // Server deletes session + expires cookie
      } catch {
/* Proceed with local cleanup even if session is already gone */
      }
      if (firebaseEnabled) await firebaseSignOut();
    },
    onSuccess: async () => {
      await qc.cancelQueries();
      qc.setQueryData(['me'], null);
      qc.removeQueries({ predicate: (query) => query.queryKey[0] !== 'me' });
      qc.getMutationCache().clear();
    },
  });
}
