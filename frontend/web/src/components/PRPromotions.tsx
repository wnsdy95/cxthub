import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';

export function PRPromotions({ repoId, canRetry }: { repoId: string; canRetry: boolean }) {
 const t = useT();
 const qc = useQueryClient();
 const key = ['pr-promotions', repoId];
 const jobs = useQuery({ queryKey: key, queryFn: ({ signal }) => api.prPromotions(repoId, signal), refetchInterval: 10000 });
 const retry = useMutation({ mutationFn: (id: string) => api.retryPRPromotion(repoId, id), onSuccess: () => qc.invalidateQueries({ queryKey: key }) });
 const labels = { waiting: t('promotion.waiting'), retrying: t('promotion.retrying'), running: t('promotion.running'), completed: t('promotion.completed'), attention: t('promotion.attention') };
 const reasons: Record<string,string> = { policy_changed: t('promotion.policy'), source_context_pending: t('promotion.source'), source_finalization_required: t('promotion.finalization'), integrity_check_failed: t('promotion.integrity'), identity_or_history_conflict: t('promotion.conflict'), repository_or_base_missing: t('promotion.missing'), temporary_failure: t('promotion.temporary'), retry_limit_reached: t('promotion.exhausted'), invalid_request: t('promotion.invalid') };
 if (jobs.isError) return <p role="alert" className="err">{t('promotion.loadError')} <button onClick={() => void jobs.refetch()}>{t('context.retryRead')}</button></p>;
 if (!jobs.data?.length) return null;
 const pending = jobs.data.filter(j => j.state !== 'completed').length;
 return <details className="pr-promotions">
  <summary>{t('promotion.title')} · {pending ? t('promotion.pending', { count: pending }) : t('promotion.completed')}</summary>
  {retry.isError && <p role="alert" className="err">{retry.error.message}</p>}
  <ul>{jobs.data.map(j => <li key={j.id}>
   <strong>PR #{j.pr.number} · {labels[j.state]}</strong>
   <span>{j.pr.head_branch} → {j.pr.base_branch}</span>
   {j.reason && <p>{reasons[j.reason] ?? t('promotion.temporary')}</p>}
   {canRetry && j.state !== 'completed' && j.state !== 'running' && <button disabled={retry.isPending} onClick={() => retry.mutate(j.id)}>{t('promotion.retry')}</button>}
  </li>)}</ul>
 </details>;
}
