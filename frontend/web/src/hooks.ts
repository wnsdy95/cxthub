// React Query — Server State (Query/Mutation).
//
// Authentication status is represented by the `me` query: success (200) → logged in, failure (401) → logged out.
// Tokens are stored in HttpOnly cookies, which JS cannot read, so cookie validity is determined by the single judge, the `me` query on the server.
import { useEffect, useMemo, useRef } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from './api';
import type { User } from './types';
import { sharedReachable, unsyncChains } from './onhold';
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
export function useWorkspaces() {
  const authed = useAuthed();
  return useQuery({ queryKey: ['workspaces'], queryFn: api.listWorkspaces, enabled: authed });
}
// Account settings: nickname is lightweight, while username changes URLs and workspace paths.
export function useUpdateMe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: { username?: string; nickname?: string; load_mode?: string; avatar?: string; locale?: string }) =>
      api.updateMe(patch),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u);
      qc.invalidateQueries({ queryKey: ['workspaces'] }); // owner_username denormalization reflected
    },
  });
}
// Workspace settings (public scope · permission policy — owner exclusive, partial PATCH).
export function useUpdateWorkspace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { wsId: string; patch: import('./types').WorkspacePatch }) => api.updateWorkspace(v.wsId, v.patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['workspaces'] }),
  });
}
// Ownership transfer (creator's sole right) — URL changes, so refresh workspace list on success.
export function useTransferWorkspace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { wsId: string; toUserId: string }) => api.transferWorkspace(v.wsId, v.toUserId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['workspaces'] }),
  });
}
// GitHub public state manual sync (owner only).
export function useSyncVisibility() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (wsId: string) => api.syncVisibility(wsId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['workspaces'] }),
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
    mutationFn: (v: { wsId: string; userId: string; role: 'owner' | 'member' }) =>
      api.updateMemberRole(v.wsId, v.userId, v.role),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['members', v.wsId] }),
  });
}
export function useRemoveMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { wsId: string; userId: string }) => api.removeMember(v.wsId, v.userId),
    onSuccess: (_r, v) => {
      qc.invalidateQueries({ queryKey: ['members', v.wsId] });
      qc.invalidateQueries({ queryKey: ['workspaces'] }); // Reflect self-exit.
    },
  });
}
export function useMembers(workspaceId: string | null) {
  return useQuery({
    queryKey: ['members', workspaceId],
    queryFn: () => api.listMembers(workspaceId as string),
    enabled: Boolean(workspaceId),
  });
}
export function useRepos(workspaceId: string | null) {
  const authed = useAuthed();
  return useQuery({
    queryKey: ['repos', workspaceId],
    queryFn: () => api.listRepos(workspaceId as string),
    enabled: authed && Boolean(workspaceId),
  });
}
// Context Browser: Branch List → Commit Log → Body(CIR). Immutable data(doc) is infinite cache.
export function useRefs(repoId: string | null) {
  return useQuery({
    queryKey: ['refs', repoId],
    queryFn: () => api.listRefs(repoId as string),
    enabled: Boolean(repoId),
  });
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
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['refs', v.repoId] }),
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
    queryFn: () => api.search(repoId as string, q),
    enabled: Boolean(repoId) && q.trim().length >= 2,
  });
}
export function useMemory(repoId: string | null, snapshotId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['memory', repoId, snapshotId],
    queryFn: () => api.getMemory(repoId as string, snapshotId as string),
    enabled: enabled && Boolean(repoId && snapshotId),
    retry: false,
    staleTime: Infinity,
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
// Push pending pointer — live state that changes with commits/pushes, refetches every 15s.
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
export function useDeletePending() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; sessionId: string }) => api.deletePending(v.repoId, v.sessionId),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['pending', v.repoId] });
    },
  });
}

// useUndismissPending — re-add dismissed pending sessions to the list (undo dismiss).
export function useUndismissPending() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; sessionId: string }) => api.undismissPending(v.repoId, v.sessionId),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['pending', v.repoId] });
    },
  });
}

// useReflog — track ref movements (git reflog equivalent). Query only when the collapsible panel is open.
export function useReflog(repoId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['reflog', repoId],
    queryFn: () => api.reflog(repoId as string),
    enabled: enabled && Boolean(repoId),
  });
}

// useJoinSnapshot — reorder session branches of the same git branch behind the head (graph drag/drop).
// On success, refresh snapshots (graft_parents update), refs (head movement/remaining session ref).
export function useJoinSnapshot() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { repoId: string; branch: string; snapshot: string; includeDescendants?: boolean }) =>
      api.joinSnapshot(v.repoId, {
        branch: v.branch,
        snapshot: v.snapshot,
        include_descendants: v.includeDescendants ?? false,
      }),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['snapshots', v.repoId, '*'] });
      void qc.invalidateQueries({ queryKey: ['refs', v.repoId] });
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
    },
  });
}

// useRepoView — single assembly point for repo-derived states shared by context/On Hold views.
// Ensure "badge count = tab row count" is guaranteed by logic (onhold.ts) and input equality, so
// exclude stash, hook capture leaves, and badge map must be created here only (review front #2).
export function useRepoView(repoId: string | null) {
  const refs = useRefs(repoId).data ?? [];
  const allData = useAllSnapshots(repoId, true).data;
  const snapshots = useMemo(() => {
    const all = (allData ?? []).filter((s) => s.branch !== '(stash)');
    // Excluding stashes is based on reachability criteria — a "(stash)" label object may exist on the server (stash-dedup trap). Just removing the label breaks the commit walk, pushing the entire history to the tip.
    const stashed = (allData ?? []).filter((s) => s.branch === '(stash)');
    if (stashed.length === 0) return all;
    const reachable = sharedReachable(refs, allData ?? []);
    return (allData ?? []).filter((s) => s.branch !== '(stash)' || reachable.has(s.id));
  }, [allData, refs]);
  const badges = useMemo(() => {
    const m = new Map<string, { name: string; kind: string }[]>();
    for (const r of refs) {
      if (r.kind !== 'branch' && r.kind !== 'tag') continue;
      const list = m.get(r.target) ?? [];
      list.push({ name: r.name, kind: r.kind });
      m.set(r.target, list);
    }
    return m;
  }, [refs]);
  // Hook capture leaves (hook: prefix) are remnants of progress state — typically excluding graph/AI bar. However, hook snapshots reachable from branch refs (absorbed into commits or directly referenced by ref) are part of the history and are displayed. Just removing the label (message prefix) breaks the commit walk, causing the head to disappear from the graph — the pin line to break and its child pending to appear orphaned (stash-dedup trap, same principle: determination based on reachability).
  const sharedIds = useMemo(() => sharedReachable(refs, snapshots), [refs, snapshots]);
  const committedSnapshots = useMemo(
    () => snapshots.filter((s) => !s.message?.startsWith('hook: ') || sharedIds.has(s.id)),
    [snapshots, sharedIds],
  );
  // Uncommitted = live pending (undismissed) target hook captures that haven't yet reached the shared timeline. Determination is based on the same criteria as the On Hold list (orphanPendings) — target reachability + unsync cluster exclusion. If any criterion is off, the list and graph classify the same session differently.
  const pendings = usePendings(repoId).data;
  const unsyncs = useUnsyncs(repoId).data;
  const uncommittedIds = useMemo(() => {
    const shared = sharedIds;
    const inCluster = new Set<string>();
    for (const c of unsyncChains(unsyncs ?? [], snapshots, shared)) {
      for (const s of c.chain) inCluster.add(s.id);
    }
    const byId = new Map(snapshots.map((s) => [s.id, s]));
    const out = new Set<string>();
    for (const p of pendings ?? []) {
      if (p.dismissed || shared.has(p.target) || inCluster.has(p.target)) continue;
      if (byId.get(p.target)?.message?.startsWith('hook: ')) out.add(p.target);
    }
    return out;
  }, [pendings, unsyncs, sharedIds, snapshots]);
  // The graph also includes uncommitted hook captures (push-pending-commit 3-layer distinction). AI bar includes only commit history. Hook snapshots reachable (same criteria as committedSnapshots) are also included — to prevent head omission.
  const graphSnapshots = useMemo(
    () =>
      snapshots.filter(
        (s) => !s.message?.startsWith('hook: ') || sharedIds.has(s.id) || uncommittedIds.has(s.id),
      ),
    [snapshots, sharedIds, uncommittedIds],
  );
  // Local predecessors (push-pending ∪ uncommitted) set and its tip (topmost — a leaf with no children). checkout -b semantics: branches have meaning only at the tip — when a new branch ref points to the tip, push commits + uncommitted state as a chain (splitting the chain).
  const localAhead = useMemo(() => {
    const shared = sharedIds;
    const ids = new Set(graphSnapshots.filter((s) => !shared.has(s.id)).map((s) => s.id));
    const hasChildAhead = new Set<string>();
    for (const s of graphSnapshots) {
      if (!ids.has(s.id)) continue;
      for (const p of [...(s.parents ?? []), ...(s.graft_parents ?? [])]) {
        if (ids.has(p)) hasChildAhead.add(p);
      }
    }
    const tips = new Set([...ids].filter((id) => !hasChildAhead.has(id)));
    return { ids, tips };
  }, [sharedIds, graphSnapshots]);
  return { refs, snapshots, badges, graphSnapshots, committedSnapshots, uncommittedIds, localAhead };
}

// ── Mutation ──────────────────────────────────────────
export function useCreateWorkspace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.createWorkspace(name),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['workspaces'] }),
  });
}
export function useCreateInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { workspaceId: string; role?: string; email?: string; expiresInDays?: number }) =>
      api.createInvite(v.workspaceId, v.email ?? '', v.role ?? 'member', v.expiresInDays ?? 0),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['invites', v.workspaceId] }),
  });
}
// Invite list/redemption — only maintainers can view (403 is disabled).
export function useInvites(workspaceId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['invites', workspaceId],
    queryFn: () => api.listInvites(workspaceId as string),
    enabled: enabled && Boolean(workspaceId),
    retry: false,
  });
}
export function useRevokeInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { workspaceId: string; token: string }) => api.revokeInvite(v.workspaceId, v.token),
    onSuccess: (_r, v) => qc.invalidateQueries({ queryKey: ['invites', v.workspaceId] }),
  });
}
export function useAcceptInvite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (token: string) => api.acceptInvite(token),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['workspaces'] }),
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
