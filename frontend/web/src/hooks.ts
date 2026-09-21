import { graphViewRows } from './graphState';
// React Query — Server State (Query/Mutation).
//
// Authentication status is represented by the `me` query: success (200) → logged in, failure (401) → logged out.
// Tokens are stored in HttpOnly cookies, which JS cannot read, so cookie validity is determined by the single judge, the `me` query on the server.
import { useEffect, useMemo, useRef } from 'react';
import { useQuery, useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from './api';
import { useRepositoryUpdates } from './useRepositoryUpdates';
import type { User } from './types';
import { firebaseEnabled, devIdpToken, firebaseEmailIdToken, firebaseEmailSignUp, firebaseGoogleIdToken, firebaseSignOut } from './auth';
import { useT } from './i18n';

// ── Authentication/Query ─────────────────────────────────────────
// me: Called once on boot to check cookie session validity. A 401 is normal (not logged in), so no retry.
// retryOnMount:false is crucial — without it, components subscribing to an errored `me` query would re-query on mount, causing status='pending' (no data) and me.isLoading to become true. This would cause an infinite loop of /me unmounting and remounting in the App's loading gate.
export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: api.me, retry: false, retryOnMount: false, staleTime: Infinity });
}
function useAuthed() {
  return Boolean(useMe().data);
}
export function useRepositories() {
  const authed = useAuthed();
  return useQuery({ queryKey: ['repositories'], queryFn: api.listRepositories, enabled: authed });
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
    onSuccess: (_result, v) => void qc.invalidateQueries({ queryKey: ['organization-members', v.organizationId] }),
  });
}
export function useRemoveOrganizationMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; userId: string; access: 'revoke' | 'retain' }) => api.removeOrganizationMember(v.organizationId, v.userId, v.access),
    onSuccess: (_result, v) => {
      void qc.invalidateQueries({ queryKey: ['organization-members', v.organizationId] });
      void qc.invalidateQueries({ queryKey: ['teams', v.organizationId] });
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
      patch: Partial<Omit<import('./types').OrganizationPolicy, 'organization_id' | 'updated_by' | 'updated_at'>>;
    }) => api.updateOrganizationPolicy(v.organizationId, v.patch),
    onSuccess: (policy) => qc.setQueryData(['organization-policy', policy.organization_id], policy),
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
export function useOrganizationAudit(organizationId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['organization-audit', organizationId],
    queryFn: () => api.listOrganizationAudit(organizationId as string),
    enabled: enabled && Boolean(organizationId),
  });
}
export function useCreateBreakGlassGrant() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { organizationId: string; repositoryId: string; reason: string; minutes: number }) =>
      api.createBreakGlassGrant(v.organizationId, v.repositoryId, v.reason, v.minutes),
    onSuccess: (_grant, v) => void qc.invalidateQueries({ queryKey: ['organization-audit', v.organizationId] }),
  });
}
// Account settings: nickname is lightweight, while username changes URLs and repository paths.
export function useUpdateMe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: { username?: string; nickname?: string; load_mode?: string; avatar?: string; locale?: string }) =>
      api.updateMe(patch),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u);
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
// Ownership transfer (creator's sole right) — URL changes, so refresh repository list on success.
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
// Context Browser: Branch List → Commit Log → Body(CIR). Immutable data(doc) is infinite cache.
export function useRefs(repoId: string | null) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['refs', repoId],
    queryFn: () => api.listRefs(repoId as string),
    enabled: Boolean(repoId),
    refetchInterval: 15_000,
  });
  const signature = (q.data ?? [])
    .map((ref) => `${ref.kind}:${ref.name}@${ref.target}`)
    .sort()
    .join(',');
  const previous = useRef(signature);
  useEffect(() => {
    if (previous.current === signature) return;
    previous.current = signature;
    void qc.invalidateQueries({ queryKey: ['snapshots', repoId, '*'] });
    void qc.invalidateQueries({ queryKey: ['reflog', repoId] });
  }, [signature, qc, repoId]);
  return q;
}
export function useAllSnapshots(repoId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['snapshots', repoId, '*'],
    queryFn: () => api.listSnapshots(repoId as string, ''),
    enabled: enabled && Boolean(repoId),
  });
}
// fork/diff — fork creates new branch ref from snapshot (invalidates refs on success),
// diff is CIR event delta between two doc hashes (read query — same pair is cached).
export function useFork() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; from: string; newBranch: string; author: { name: string; email: string } }) =>
      api.fork(v.repoId, v.from, v.newBranch, v.author),
    onSuccess: (_r, v) => {
      void qc.invalidateQueries({ queryKey: ['refs', v.repoId] });
      void qc.invalidateQueries({ queryKey: ['repo-view', v.repoId] });
    },
  });
}
export function useSnapDiff(repoId: string | null, left: string | null, right: string | null) {
  return useQuery({
    queryKey: ['diff', repoId, left, right],
    queryFn: () => api.diff(repoId as string, left as string, right as string),
    enabled: Boolean(repoId && left && right && left !== right),
  });
}
// Search — Queries less than 2 characters are 422 by server, so disabled on client. Same (repo,q) is cached.
export function useSearch(repoId: string | null, q: string) {
  return useQuery({
    queryKey: ['search', repoId, q],
    queryFn: ({ signal }) => api.search(repoId as string, q, signal),
    enabled: Boolean(repoId) && q.trim().length >= 2,
  });
}
export function useMemory(repoId: string | null, memoryHash: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['memory-object', repoId, memoryHash],
    queryFn: () => api.getMemoryObject(repoId as string, memoryHash as string),
    enabled: enabled && Boolean(repoId && memoryHash),
    retry: false,
    staleTime: Infinity,
  });
}
export function useDocPages(repoId: string | null, hash: string | null, base?: string, start = -1) {
  return useInfiniteQuery({
    queryKey: ['doc-events', repoId, hash, base ?? '', start],
    queryFn: ({ pageParam, signal }) => api.getDocEvents(repoId!, hash!, base, pageParam, signal),
    initialPageParam: start,
    getNextPageParam: (last) => last.next < 0 ? undefined : last.next,
    enabled: Boolean(repoId && hash),
    staleTime: Infinity,
    gcTime: 5 * 60_000,
  });
}

export function useDoc(repoId: string | null, hash: string | null) {
  return useQuery({
    queryKey: ['doc', repoId, hash],
    queryFn: () => api.getDoc(repoId as string, hash as string),
    enabled: Boolean(repoId && hash),
    staleTime: Infinity, // content-addressed — same hash always same content
  });
}

// Pointer(pending/unsync) response change invalidates refs/snapshots — pointer only 15s.
// Polling and commits/refs are static cache, preventing mismatch window when "On Hold" disappears but not in Context tab (review front #3).
function useInvalidateOnChange(repoId: string | null, signature: string) {
  const qc = useQueryClient();
  const prev = useRef(signature);
  useEffect(() => {
    if (prev.current === signature) return;
    prev.current = signature;
    void qc.invalidateQueries({ queryKey: ['refs', repoId] });
    void qc.invalidateQueries({ queryKey: ['snapshots', repoId, '*'] });
  }, [signature, qc, repoId]);
}

// In-progress context pointer — live state updated by hook capture, refetches every 15s.
export function usePendings(repoId: string | null) {
  const q = useQuery({
    queryKey: ['pending', repoId],
    queryFn: () => api.listPending(repoId as string),
    enabled: Boolean(repoId),
    refetchInterval: 15_000,
  });
  useInvalidateOnChange(
    repoId,
    'p:' + ((q.data ?? []).map((p) => p.session_id + '@' + p.target).sort().join(',')),
  );
  return q;
}
// Mutable push-wait pointer; refetch every 15s. It does not prove process liveness.
export function useUnsyncs(repoId: string | null) {
  const q = useQuery({
    queryKey: ['unsync', repoId],
    queryFn: () => api.listUnsync(repoId as string),
    enabled: Boolean(repoId),
    refetchInterval: 15_000,
  });
  useInvalidateOnChange(
    repoId,
    'u:' + ((q.data ?? []).map((u) => u.user + '/' + u.branch + '@' + u.target).sort().join(',')),
  );
  return q;
}

// useUndismissPending — re-add dismissed pending sessions to the list (undo dismiss).
export function useUndismissPending() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; sessionId: string }) => api.undismissPending(v.repoId, v.sessionId),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['pending', v.repoId] });
      void qc.invalidateQueries({ queryKey: ['repo-view', v.repoId] });
    },
  });
}

// Ref movements supply graph history evidence as well as the reflog panel.
export function useReflog(repoId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['reflog', repoId],
    queryFn: () => api.reflog(repoId as string),
    enabled: enabled && Boolean(repoId),
  });
}

// useJoinSnapshot — reorder session branches of the same git branch behind the head (graph drag/drop).
// On success, refresh snapshots (graft_parents update), refs (head movement/remaining session ref).
export function useJoinPreview(repoId: string | null | undefined, snapshot: string | null, branch?: string) {
  return useQuery({
    queryKey: ['join-preview', repoId, snapshot, branch ?? ''],
    enabled: Boolean(repoId && snapshot),
    queryFn: ({signal}) => api.joinPreview(repoId!, snapshot!, branch, signal),
    staleTime: 0,
    refetchOnWindowFocus: false,
    retry: false,
  });
}
export function useJoinSnapshot() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: {repoId: string; preview: import('./types').JoinPreview; includeDescendants: boolean}) =>
      api.joinSnapshot(v.repoId, {
        branch: v.preview.branch,
        branch_id: v.preview.branch_id,
        snapshot: v.preview.snapshot,
        include_descendants: v.includeDescendants,
        expected_head: v.preview.expected_head!,
        plan_revision: (v.includeDescendants ? v.preview.all_revision : v.preview.only_revision)!,
      }),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['snapshots', v.repoId, '*'] });
      void qc.invalidateQueries({ queryKey: ['repo-view', v.repoId] });
      void qc.invalidateQueries({ queryKey: ['refs', v.repoId] });
      void qc.invalidateQueries({ queryKey: ['join-preview', v.repoId] });
    },
  });
}

// useDismissPending — hide pending sessions from the list (data deletion is not performed, sticky).
export function useDismissPending() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; sessionId: string }) => api.dismissPending(v.repoId, v.sessionId),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['pending', v.repoId] });
      void qc.invalidateQueries({ queryKey: ['repo-view', v.repoId] });
    },
  });
}

// One server generation shared by the context and On Hold views.
export function useRepoView(repoId: string | null, _primaryBranch?: string) {
  useRepositoryUpdates(repoId);
  const viewQuery = useQuery({queryKey:['repo-view',repoId],queryFn:()=>api.repositoryView(repoId!),retry:false,
    enabled:Boolean(repoId),refetchOnWindowFocus:false});
  const view = viewQuery.data;
  const rows = useMemo(()=>graphViewRows(view),[view]);
  return {...rows,refs:view?.refs ?? [],reflog:view?.reflog ?? [],history:view?.history ?? [],graphState:view?.graph,
    pendings:view?.pending ?? [],unsyncs:view?.unsync ?? [],semantics:view?.semantics,historyError:viewQuery.isError,
    graphLoading:viewQuery.isPending,graphError:viewQuery.error?.message,retryGraph:()=>{void viewQuery.refetch();}};
}

export function useGraphPosition(repoId: string | null | undefined, position: string, revision?: import('./types').RepositoryRevision) {
  const qc = useQueryClient();
  const q = useQuery({queryKey:['graph-position',repoId,position,revision?.graph,revision?.pending,revision?.evidence],
    queryFn:({signal})=>api.graphState(repoId!,position,signal),enabled:Boolean(repoId && position && revision),retry:false,refetchOnWindowFocus:false});
  const matches = q.data && revision && q.data.revision.graph===revision.graph && q.data.revision.pending===revision.pending && (q.data.revision.evidence??'0')===(revision.evidence??'0');
  useEffect(()=>{if(q.data && !matches) void qc.invalidateQueries({queryKey:['repo-view',repoId]});},[q.data,matches,qc,repoId]);
  return {...q,data:matches?q.data:undefined};
}

// ── Mutation ──────────────────────────────────────────
export function useCreateRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.createRepository(name),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repositories'] }),
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
    onSuccess: () => qc.invalidateQueries({ queryKey: ['repositories'] }),
  });
}

export function useEnableContextProtocol() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (repoId: string) => api.enableContextProtocol(repoId),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['repos'] });
      void qc.invalidateQueries({ queryKey: ['refs'] });
      void qc.invalidateQueries({ queryKey: ['repo-view'] });
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
      default_branch?: string;
      protect_default?: boolean;
    }) =>
      api.updateAbout(v.repoId, {
        description: v.description,
        website: v.website,
        topics: v.topics,
        default_branch: v.default_branch,
        protect_default: v.protect_default,
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
export type LoginInput =
  | { mode: 'dev'; email: string; name: string }
  | { mode: 'email'; email: string; password: string }
  | { mode: 'google' };

export function useLogin() {
  const qc = useQueryClient();
  const t = useT();
  return useMutation({
    mutationFn: async (input: LoginInput) => {
      let idp: string;
      if (input.mode === 'google') idp = await firebaseGoogleIdToken(t);
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
    onSuccess: () => qc.clear(), // Clear me cache and all others → Gate transitions to Login
  });
}
