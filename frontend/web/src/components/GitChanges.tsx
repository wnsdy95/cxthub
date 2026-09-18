import { useState } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';

// The server owns verification and coverage. This component presents its facts;
// graph visibility and selected branch never reinterpret a historical result.
export function GitChanges({repoId, canRetry}: {repoId: string; canRetry: boolean}) {
 const t = useT();
 const [open, setOpen] = useState(false);
 const qc = useQueryClient();
 const key = ['git-changes', repoId];
 const jobs = useInfiniteQuery({queryKey: key, initialPageParam: '',
  queryFn: ({pageParam, signal}) => api.gitChanges(repoId, pageParam, signal),
  getNextPageParam: page => page.next_cursor || undefined, enabled: open, retry: false});
 const retry = useMutation({mutationFn: (id: string) => api.retryGitChange(repoId, id),
  onSuccess: () => qc.invalidateQueries({queryKey: key})});
 const labels = {waiting: t('promotion.waiting'), running: t('promotion.running'), retrying: t('promotion.retrying'), attention: t('promotion.attention'), completed: t('gitChanges.verified')};
 const coverage = {full: t('gitChanges.full'), partial: t('gitChanges.partial'), unverified: t('gitChanges.unverified')};
 const reasons: Record<string, string> = {
  temporary_provider_or_storage_failure: t('promotion.temporary'), integrity_check_failed: t('promotion.integrity'),
  invalid_or_ambiguous_git_evidence: t('gitChanges.ambiguous'), origin_or_lease_changed: t('gitChanges.changed'),
  repository_missing: t('promotion.missing'), retry_limit_reached: t('promotion.exhausted'),
  incomplete_git_evidence: t('gitChanges.incomplete'), target_not_in_candidate_ancestry: t('gitChanges.unrelated'),
  no_exact_inverse: t('gitChanges.noInverse'), remaining_paths_need_review: t('gitChanges.partial'),
  target_has_no_changes: t('gitChanges.noChanges'), candidate_has_no_parent: t('gitChanges.noParent'),
 };
 const rows = jobs.data?.pages.flatMap(page => page.items) ?? [];
 return <details className="git-changes" open={open} onToggle={e => setOpen(e.currentTarget.open)}>
  <summary>{t('gitChanges.title')}</summary>
  {open && <>
   <p>{t('gitChanges.scope')}</p>
   {jobs.isPending && <p role="status">{t('gitChanges.loading')}</p>}
   {jobs.isError && <p role="alert" className="err">{t('gitChanges.loadError')} <button onClick={() => void jobs.refetch()}>{t('context.retryRead')}</button></p>}
   {retry.isError && <p role="alert" className="err">{retry.error.message}</p>}
   {jobs.isSuccess && !rows.length && <p>{t('gitChanges.empty')}</p>}
   <ul>{rows.map(j => <li key={j.id}>
    <strong>{j.coverage ? coverage[j.coverage] : labels[j.state]}</strong>
    <span><code title={j.request.commit}>{j.request.commit.slice(0, 10)}</code> ↶ <code title={j.request.target}>{j.request.target.slice(0, 10)}</code></span>
    {j.coverage && <span>{t('gitChanges.paths', {verified: j.verified_paths, unknown: j.unverified_paths})}</span>}
    {j.reason && <p>{reasons[j.reason] ?? t('gitChanges.ambiguous')}</p>}
    <time dateTime={j.updated_at}>{new Date(j.updated_at).toLocaleString()}</time>
    {canRetry && ['attention', 'retrying'].includes(j.state) && <button disabled={retry.isPending} onClick={() => retry.mutate(j.id)}>{t('promotion.retry')}</button>}
   </li>)}</ul>
   {jobs.hasNextPage && <button disabled={jobs.isFetchingNextPage} onClick={() => void jobs.fetchNextPage()}>{t('gitChanges.more')}</button>}
  </>}
 </details>;
}
