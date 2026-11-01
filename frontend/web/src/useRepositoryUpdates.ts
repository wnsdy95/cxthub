import { useEffect } from 'react';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { api, ApiError } from './api';
import { capacityReadRetry, capacityRetryDelay, pendingStateReadRetry, retryCapacityRead, retryPendingStateRead } from './readRetry';
import type { PendingProjectionStatus, PendingStateView, RepositoryRevision, RepositoryView } from './types';
import { mergePendingView, parseRevision, pendingViewNeedsFull, revisionCovers, samePendingProjection, subscribeRepository } from './repositoryUpdates';
import { pendingProjectionMatches } from './pendingState';
import { graphGenerationMatches, graphRefreshTarget, validateGraphRead } from './graphReadState';

export function retryGraphRead(failureCount: number, error: unknown): boolean {
  return retryCapacityRead(failureCount, error) || (failureCount < 9 && error instanceof ApiError && error.status === 503 &&
    error.code === 'graph_projection_pending' && (error.retryAfterMs === undefined || error.retryAfterMs <= 10_000));
}

export const graphReadRetry = {
  retry: (failureCount: number, error: Error) => retryGraphRead(failureCount, error),
  retryDelay: (attempt: number, error: Error) => Math.min(capacityRetryDelay(attempt, error), 10_000),
};

function sameRevision(current: RepositoryRevision | undefined, wanted: RepositoryRevision | null) {
  return Boolean(current && wanted && revisionCovers(current, wanted) && revisionCovers(wanted, current));
}

function sameNotice(current: RepositoryRevision | undefined, wanted: RepositoryRevision | null) {
  return sameRevision(current, wanted) && samePendingProjection(current?.pending_projection, wanted?.pending_projection);
}

function pendingMatches(current: PendingStateView | undefined, wanted: RepositoryRevision) {
  const status = wanted.pending_projection;
  return status && status.state !== 'disabled'
    ? pendingProjectionMatches(current, status) : sameRevision(current?.revision, wanted);
}

export async function readRepositoryPending(repo: string, qc: QueryClient, signal?: AbortSignal) {
  const live = await api.pendingState(repo, signal);
  const status = qc.getQueryData<RepositoryRevision>(['repository-notice', repo])?.pending_projection;
  if (status?.state === 'ready' && !pendingProjectionMatches(live, status)) {
    throw new Error('On Hold projection does not match the published generation. Retry the read.');
  }
  return live;
}

export async function readRepositoryGraph(repo: string, qc: QueryClient, signal?: AbortSignal) {
  const noticeKey = ['repository-notice', repo];
  const access = qc.getQueryState(noticeKey);
  if (access?.error) throw access.error;
  const view = await api.repositoryView(repo, signal);
  const latest = qc.getQueryState(noticeKey);
  if (latest?.error) throw latest.error;
  if ((access?.errorUpdateCount ?? 0) !== (latest?.errorUpdateCount ?? 0)) throw new Error('Repository access changed during the graph read');
  return validateGraphRead(view, qc.getQueryData<RepositoryRevision>(noticeKey)?.graph_projection);
}

export function useRepositoryUpdates(repo: string | null) {
  const qc = useQueryClient();
  useEffect(() => repo ? startRepositoryUpdates(repo, qc) : undefined, [repo, qc]);
}

export function startRepositoryUpdates(repo: string, qc: QueryClient) {
  const key = ['repo-view', repo];
  const pendingKey = ['pending-state', repo, 'on-hold-v1'];
  const noticeKey = ['repository-notice', repo];
  let disposed = false, busy = false, pendingBusy = false, wanted: RepositoryRevision | null = null;
  let pendingAttempt: { notice: RepositoryRevision; count: number } | undefined;
  let graphAttempt: { target: string; count: number; terminal: boolean } | undefined;
  let disabledSource: PendingProjectionStatus | undefined;
  let sourceIncarnation = (qc.getQueryData<RepositoryRevision>(noticeKey)?.graph_projection?.source_epoch ??
    qc.getQueryData<RepositoryView>(key)?.read_generation?.source_epoch ??
    qc.getQueryData<RepositoryRevision>(noticeKey)?.pending_projection?.source_epoch ??
    qc.getQueryData<PendingStateView>(pendingKey)?.read_generation?.source_epoch)?.slice(0, 36);
  let sourceVersion = 0, fullSourceVersion = 0;
  let fullAttempts = 0, pendingRefresh = 0, pendingApplied = 0, revisionRequest = 0;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let delay = 15_000;
  let evidence = 0n;
  let unsubscribe: (() => void) | undefined;
  const clearRetry = () => { if (retry) clearTimeout(retry); retry = undefined; };
  const pendingCurrent = (current: PendingStateView | undefined, notice: RepositoryRevision) => pendingMatches(current, notice) &&
    (notice.pending_projection?.state !== 'disabled' || samePendingProjection(disabledSource, notice.pending_projection));
  const denied = () => Boolean(qc.getQueryState(noticeKey)?.error);
  const accessVersion = () => qc.getQueryState(noticeKey)?.errorUpdateCount ?? 0;
  const active = (version = accessVersion()) => !disposed && !document.hidden && !denied() && version === accessVersion();
  const requireFull = () => { sourceVersion++; fullAttempts = 0; graphAttempt = undefined; };
  const deny = (error: unknown) => {
    if (!(error instanceof ApiError) || ![401, 403].includes(error.status)) return;
    qc.getQueryCache().build<RepositoryRevision, Error>(qc, {queryKey: noticeKey}).setState({status: 'error', error,
      errorUpdateCount: accessVersion() + 1});
    void qc.cancelQueries({queryKey: key, exact: true});
    void qc.cancelQueries({queryKey: pendingKey, exact: true});
  };
  const observeSource = (epoch: string | undefined) => {
    if (!epoch) return;
    const incarnation = epoch.slice(0, 36);
    if (sourceIncarnation && sourceIncarnation !== incarnation) requireFull();
    sourceIncarnation = incarnation;
  };
  // React Query owns cancellation: another observer may still need this view.
  const full = () => qc.fetchQuery({queryKey: key, queryFn: ({signal}) => readRepositoryGraph(repo, qc, signal), staleTime: 0, ...graphReadRetry});
  const pending = async () => {
    if (pendingBusy || !active() || !wanted) return;
    const projection = wanted.pending_projection;
    if (projection?.state === 'catching_up' || projection?.state === 'needs_attention') return;
    const current = qc.getQueryState<PendingStateView, Error>(pendingKey);
    const refresh = pendingRefresh !== pendingApplied;
    if (!refresh && pendingCurrent(current?.data, wanted) && !(current?.status === 'error' &&
        (projection?.state === 'ready' || retryPendingStateRead(0, current.error)))) return;
    if (!sameNotice(pendingAttempt?.notice, wanted)) pendingAttempt = { notice: wanted, count: 0 };
    const attempt = pendingAttempt!;
    if ((projection || refresh) && attempt.count >= 2) return;
    const requested = wanted;
    const requestedAccess = accessVersion(), requestedRefresh = pendingRefresh;
    const joined = current?.fetchStatus === 'fetching';
    const joinedFollowup = joined && (refresh || Boolean(current?.data && !pendingCurrent(current.data, requested)));
    if (!joined) attempt.count++;
    pendingBusy = true;
    try {
      const live = await qc.fetchQuery({queryKey: pendingKey, queryFn: ({signal}) => readRepositoryPending(repo, qc, signal), staleTime: 0,
        ...(projection?.state === 'ready' ? capacityReadRetry : pendingStateReadRetry)});
      if (!active(requestedAccess)) return;
      if (!joined) pendingApplied = requestedRefresh;
      if (!joined && projection?.state === 'disabled') disabledSource = projection;
      if (!wanted.pending_projection && !wanted.graph_projection) {
        observeSource(live.read_generation?.source_epoch);
        if (sourceVersion !== fullSourceVersion) void sync();
      }
      if (sameNotice(requested, wanted) && !pendingCurrent(live, wanted) && !joinedFollowup) {
        if (projection && attempt.count >= 2) {
          qc.getQueryCache().find({queryKey: pendingKey, exact: true})?.setState({status: 'error',
            error: new Error('On Hold projection does not match the published generation. Retry the read.')});
        }
        recover();
      }
    } catch (error) {
      if (active(requestedAccess)) {
        deny(error);
        if (sameNotice(requested, wanted) && !joinedFollowup) recover();
      }
    }
    finally {
      pendingBusy = false;
      if (active() && (!sameNotice(requested, wanted) || requestedRefresh !== pendingRefresh ||
          (joinedFollowup && (!pendingCurrent(qc.getQueryData(pendingKey), wanted!) || refresh)))) void pending();
    }
  };
  const readRevision = async () => {
    const request = ++revisionRequest;
    const requestedAccess = accessVersion();
    try {
      const revision = parseRevision(await api.repositoryRevision(repo));
      if (!revision) throw new Error('Invalid repository revision');
      if (disposed || document.hidden || request !== revisionRequest || requestedAccess !== accessVersion()) return;
      if (denied()) {
        pendingRefresh++;
        pendingAttempt = undefined;
        requireFull();
      }
      qc.setQueryData<RepositoryRevision>(noticeKey, revision);
      await changed(revision);
    }
    catch (error) {
      if (disposed || document.hidden || request !== revisionRequest || requestedAccess !== accessVersion()) return;
      deny(error);
      // A rolling deployment may still route to a pre-revision backend.
      // Do not turn authorization or transient errors into expensive fetches.
      if (error instanceof ApiError && [404, 405, 501].includes(error.status)) {
        if (active()) await full();
      } else throw error;
    }
  };
  const recover = () => {
    if (disposed || document.hidden || retry) return;
    retry = setTimeout(async () => {
      retry = undefined;
      try { await readRevision(); } catch { /* bounded retry below */ }
      recover();
    }, delay);
    delay = Math.min(delay * 2, 120_000);
  };
  const sync = async () => {
    if (busy || !active() || !wanted) return;
    const requested = wanted;
    const requestedAccess = accessVersion();
    const requestedSourceVersion = sourceVersion;
    const sourceChanged = requestedSourceVersion !== fullSourceVersion;
    const graphProjection = requested.graph_projection;
    const graphTarget = graphRefreshTarget(graphProjection);
    if (graphTarget && graphProjection) {
      if (!graphProjection.complete || graphProjection.state === 'needs_attention') return;
      if (graphGenerationMatches(qc.getQueryData(key), graphProjection) && !sourceChanged) return;
      if (graphAttempt?.target !== graphTarget) graphAttempt = { target: graphTarget, count: 0, terminal: false };
      if (graphAttempt.count >= 2 || graphAttempt.terminal) return;
      const attempt = graphAttempt;
      const joined = qc.getQueryState(key)?.fetchStatus === 'fetching';
      if (!joined) attempt.count++;
      busy = true;
      try {
        const next = validateGraphRead(await full(), wanted.graph_projection);
        if (!active(requestedAccess)) return;
        if (!joined) fullSourceVersion = requestedSourceVersion;
        if (!graphGenerationMatches(next, wanted.graph_projection!) && graphTarget === graphRefreshTarget(wanted.graph_projection)) recover();
      } catch (error) {
        if (active(requestedAccess)) {
          deny(error);
          attempt.terminal = error instanceof ApiError && error.code === 'graph_projection_failed';
          if (!attempt.terminal) recover();
        }
      } finally {
        busy = false;
        if (active() && (graphTarget !== graphRefreshTarget(wanted.graph_projection) || requestedSourceVersion !== sourceVersion ||
            (joined && !attempt.terminal && !graphGenerationMatches(qc.getQueryData(key), wanted.graph_projection!)))) void sync();
      }
      return;
    }
    if (sourceChanged && fullAttempts >= 2) return;
    let joined = false, joinedFollowup = false;
    const readFull = async () => {
      joined = qc.getQueryState(key)?.fetchStatus === 'fetching';
      const cached = qc.getQueryData<RepositoryView>(key)?.revision;
      joinedFollowup = joined && (sourceChanged || Boolean(cached && !sameRevision(cached, requested)));
      if (sourceChanged && !joined) fullAttempts++;
      const live = await full();
      if (active(requestedAccess) && !joined) fullSourceVersion = requestedSourceVersion;
      return live;
    };
    busy = true;
    try {
      const current = qc.getQueryData<RepositoryView>(key);
      if (sameRevision(current?.revision, wanted) && !sourceChanged) return;
      if (sourceChanged || current?.read_generation || !current?.revision || current.revision.graph !== wanted.graph || !revisionCovers(wanted, current.revision)) {
        void qc.invalidateQueries({queryKey: ['git-changes', repo]});
        void qc.invalidateQueries({queryKey: ['git-scans', repo]});
        void qc.invalidateQueries({queryKey: ['code-applicability', repo]});
        void qc.invalidateQueries({queryKey: ['effective-memory', repo]});
        void qc.invalidateQueries({queryKey: ['memory-positions', repo]});
        const next = await readFull();
        if (!active(requestedAccess) || !next.revision) return;
      } else {
        const live = await api.pendingView(repo);
        if (!active(requestedAccess)) return;
        const latest = qc.getQueryData<RepositoryView>(key);
        if (!latest || pendingViewNeedsFull(latest, live)) await readFull();
        else qc.setQueryData<RepositoryView>(key, old => old ? mergePendingView(old, live) : old);
      }
      if (!active(requestedAccess)) return;
      const next = qc.getQueryData<RepositoryView>(key)?.revision;
      if (sameRevision(requested, wanted) && !sameRevision(next, wanted) && !joinedFollowup) recover();
    } catch (error) {
      if (active(requestedAccess)) {
        deny(error);
        if (sameRevision(requested, wanted) && !joinedFollowup) recover();
      }
    }
    finally {
      busy = false;
      if (active() && (!sameRevision(requested, wanted) || graphRefreshTarget(requested.graph_projection) !== graphRefreshTarget(wanted.graph_projection) || requestedSourceVersion !== sourceVersion ||
          (joinedFollowup && (sourceChanged || !sameRevision(qc.getQueryData<RepositoryView>(key)?.revision, wanted))))) void sync();
    }
  };
  const changed = async (r: RepositoryRevision) => {
    if (!active()) return;
    if (!sameNotice(wanted ?? undefined, r)) pendingAttempt = undefined;
    if (wanted?.pending_projection?.state !== r.pending_projection?.state) disabledSource = undefined;
    const nextEvidence = BigInt(r.evidence ?? '0');
    if (nextEvidence !== evidence) {
      evidence = nextEvidence;
      for (const name of ['git-changes', 'git-scans', 'code-applicability', 'effective-memory']) void qc.invalidateQueries({queryKey: [name, repo]});
    }
    const previousGraph = wanted?.graph_projection ?? qc.getQueryData<RepositoryRevision>(noticeKey)?.graph_projection;
    const previousIncarnation = previousGraph?.source_epoch.slice(0, 36) ?? qc.getQueryData<RepositoryView>(key)?.read_generation?.source_epoch.slice(0, 36);
    if (r.graph_projection && r.graph_projection.state !== 'disabled' &&
        (!r.graph_projection.complete || r.graph_projection.state === 'needs_attention' ||
          (previousIncarnation && previousIncarnation !== r.graph_projection.source_epoch.slice(0, 36)))) {
      void qc.cancelQueries({queryKey: key, exact: true});
    }
    wanted = r;
    qc.setQueryData<RepositoryRevision>(noticeKey, r);
    observeSource(r.graph_projection?.source_epoch ?? r.pending_projection?.source_epoch);
    if (r.pending_projection?.state === 'catching_up' || r.pending_projection?.state === 'needs_attention') {
      void qc.cancelQueries({queryKey: pendingKey, exact: true});
    }
    void pending();
    await sync();
  };
  const visible = () => {
    unsubscribe?.(); unsubscribe = undefined; clearRetry();
    if (document.hidden || disposed) return;
    unsubscribe = subscribeRepository(api.repositoryChangesURL(repo), {
      changed: r => { if (!active()) return; clearRetry(); delay = 15_000; void changed(r); },
      failed: recover,
    });
    // Also works if an intermediary blocks streaming entirely.
    void readRevision().catch(recover);
  };
  visible(); document.addEventListener('visibilitychange', visible);
  return () => { disposed = true; unsubscribe?.(); clearRetry(); document.removeEventListener('visibilitychange', visible); };
}
