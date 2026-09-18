import {useState} from 'react';
import {useInfiniteQuery, useMutation, useQueryClient} from '@tanstack/react-query';
import {api} from '../api';
import {useT} from '../i18n';

export function GitScans({repoId, canRetry}: {repoId: string; canRetry: boolean}) {
 const t = useT();
 const [open, setOpen] = useState(false);
 const qc = useQueryClient();
 const key = ['git-scans', repoId];
 const jobs = useInfiniteQuery({queryKey: key, initialPageParam: '',
  queryFn: ({pageParam, signal}) => api.gitScans(repoId, pageParam, signal),
  getNextPageParam: page => page.next_cursor || undefined, enabled: open, retry: false});
 const retry = useMutation({mutationFn: (id: string) => api.retryGitScan(repoId, id),
  onSuccess: () => qc.invalidateQueries({queryKey: key})});
 const labels = {waiting: t('promotion.waiting'), running: t('promotion.running'), retrying: t('promotion.retrying'), attention: t('promotion.attention'), completed: t('gitScans.completed')};
 const reasons: Record<string, string> = {temporary_provider_or_storage_failure: t('promotion.temporary'), integrity_check_failed: t('promotion.integrity'), invalid_or_ambiguous_git_evidence: t('gitChanges.ambiguous'), origin_or_lease_changed: t('gitChanges.changed')};
 const heads = jobs.data?.pages[0].reconciliation;
 const rows = jobs.data?.pages.flatMap(p => p.items) ?? [];
 return <details className="git-scans" open={open} onToggle={e => setOpen(e.currentTarget.open)}>
  <summary>{t('gitScans.title')}</summary>
  {open && <>
   <p>{t('gitScans.scope')}</p>
   {heads && <p className="git-head-progress">{t('gitScans.heads', {page: heads.page})} · {heads.state === 'completed' ? t('gitScans.checked') : labels[heads.state]}{heads.reason && <> — {reasons[heads.reason] ?? t('gitChanges.ambiguous')}</>}</p>}
   {jobs.isPending && <p role="status">{t('gitChanges.loading')}</p>}
   {jobs.isError && <p role="alert">{t('gitChanges.loadError')} <button onClick={() => void jobs.refetch()}>{t('context.retryRead')}</button></p>}
   {retry.isError && <p role="alert">{retry.error.message}</p>}
   {jobs.isSuccess && !rows.length && <p>{t('gitScans.empty')}</p>}
   <ul>{rows.map(j => <li key={j.id}>
    <strong>{labels[j.state]}</strong> <code title={j.commit}>{j.commit.slice(0, 10)}</code>
    <span>{j.tree_indexed ? t('codeState.treeReady') : t('codeState.treePending')}</span>
    <span>{j.indexed ? t('gitScans.indexed') : t('gitScans.reading')}</span>
    {j.reason && <p>{reasons[j.reason] ?? t('gitChanges.ambiguous')}</p>}
    <time dateTime={j.updated_at}>{new Date(j.updated_at).toLocaleString()}</time>
    {canRetry && ['attention', 'retrying'].includes(j.state) && <button disabled={retry.isPending} onClick={() => retry.mutate(j.id)}>{t('promotion.retry')}</button>}
   </li>)}</ul>
   {jobs.hasNextPage && <button disabled={jobs.isFetchingNextPage} onClick={() => void jobs.fetchNextPage()}>{t('gitChanges.more')}</button>}
  </>}
 </details>;
}
