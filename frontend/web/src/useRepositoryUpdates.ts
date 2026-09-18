import { useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from './api';
import type { RepositoryRevision, RepositoryView } from './types';
import { mergePendingView, pendingViewNeedsFull, revisionCovers, subscribeRepository } from './repositoryUpdates';

export function useRepositoryUpdates(repo: string | null) {
  const qc = useQueryClient();
  useEffect(() => {
    if (!repo) return;
    const key = ['repo-view', repo];
    let disposed = false, busy = false, wanted: RepositoryRevision | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = 15_000;
    let unsubscribe: (() => void) | undefined;
    const clearRetry = () => { if (retry) clearTimeout(retry); retry = undefined; };
    const full = () => qc.fetchQuery({queryKey: key, queryFn: () => api.repositoryView(repo), staleTime: 0});
    const readRevision = async () => {
      try { await changed(await api.repositoryRevision(repo)); }
      catch (error) {
        // A rolling deployment may still route to a pre-revision backend.
        // Do not turn authorization or transient errors into expensive fetches.
        if (error instanceof ApiError && [404, 405, 501].includes(error.status)) {
          if (!disposed && !document.hidden) await full();
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
      if (busy || disposed || document.hidden) return;
      busy = true;
      try {
        while (wanted && !disposed && !document.hidden) {
          let current = qc.getQueryData<RepositoryView>(key);
          if (!current) current = await full();
          if (current.revision && revisionCovers(current.revision, wanted)) break;
          if (!current.revision || current.revision.graph !== wanted.graph) {
            void qc.invalidateQueries({queryKey: ['git-changes', repo]});
            void qc.invalidateQueries({queryKey: ['git-scans', repo]});
            const next = await full();
            if (!next.revision) break; // rolling upgrade: old backend
          } else {
            const live = await api.pendingView(repo);
            if (disposed) break;
            const latest = qc.getQueryData<RepositoryView>(key);
            if (!latest || pendingViewNeedsFull(latest, live)) await full();
            else qc.setQueryData<RepositoryView>(key, old => old ? mergePendingView(old, live) : old);
          }
        }
      } catch { recover(); }
      finally { busy = false; }
    };
    const changed = async (r: RepositoryRevision) => { wanted = r; await sync(); };
    const visible = () => {
      unsubscribe?.(); unsubscribe = undefined; clearRetry();
      if (document.hidden || disposed) return;
      unsubscribe = subscribeRepository(api.repositoryChangesURL(repo), {
        changed: r => { clearRetry(); delay = 15_000; void changed(r); },
        failed: recover,
      });
      // Also works if an intermediary blocks streaming entirely.
      void readRevision().catch(recover);
    };
    visible(); document.addEventListener('visibilitychange', visible);
    return () => { disposed = true; unsubscribe?.(); clearRetry(); document.removeEventListener('visibilitychange', visible); };
  }, [repo, qc]);
}
