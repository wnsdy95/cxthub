import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';

const reasons = ['configuration_unavailable', 'destination_disabled', 'destination_changed', 'invalid_destination', 'attempts_exhausted', 'transport_failed', 'http_retryable', 'http_rejected', 'encoding_failed'] as const;

export function NotificationHistory({ workspace }: { workspace: string }) {
  const t = useT();
  const qc = useQueryClient();
  const query = useQuery({ queryKey: ['notifications', workspace], queryFn: ({ signal }) => api.notifications(workspace, signal), refetchInterval: 10_000 });
  const retry = useMutation({ mutationFn: (id: string) => api.retryNotification(workspace, id), onSuccess: () => qc.invalidateQueries({ queryKey: ['notifications', workspace] }) });
  return <section className="settings-upload notification-history" aria-label={t('notifications.title')}>
    <h3>{t('notifications.title')}</h3>
    <p className="hint">{t('notifications.hint')}</p>
    <p className="hint">{t('notifications.retryHint')}</p>
    {query.isError && <p role="alert" className="err">{t('notifications.unavailable')}</p>}
    {retry.isError && <p role="alert" className="err">{t('notifications.retryFailed')}</p>}
    {query.data?.length === 0 && <p className="hint">{t('notifications.empty')}</p>}
    <div className="notification-list">{query.data?.map(job => {
      const reason = reasons.find(r => r === job.reason);
      return <article key={job.id} className="notification-entry">
        <strong>{t(`notifications.${job.state}`)}</strong>
        <p>{job.text}</p>
        <small><time>{new Date(job.created_at).toLocaleString()}</time> · {t('notifications.attempts')}: {job.attempts}{job.http_status ? ` · HTTP ${job.http_status}` : ''}</small>
        {reason && <p>{t('notifications.reason')}: {t(`notifications.${reason}`)}</p>}
        {job.state === 'retrying' && <p>{t('notifications.next')}: {new Date(job.next_attempt).toLocaleString()}</p>}
        <code>{job.id}</code>
        {(job.state === 'attention' || job.state === 'retrying') && <button type="button" className="ghost mini" disabled={retry.isPending} onClick={() => retry.mutate(job.id)}>{t('notifications.retry')}</button>}
      </article>;
    })}</div>
  </section>;
}
