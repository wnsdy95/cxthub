import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';

function bytes(value: number) {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let index = 0;
  while (Math.abs(value) >= 1024 && index < units.length - 1) { value /= 1024; index++; }
  return `${value.toLocaleString(undefined, { maximumFractionDigits: 2 })} ${units[index]}`;
}
export function StorageUsage({ namespace, canReconcile }: { namespace: string; canReconcile: boolean }) {
  const t = useT();
  const [month, setMonth] = useState(() => new Date().toISOString().slice(0, 7));
  const qc = useQueryClient();
  const usage = useQuery({ queryKey: ['storage', namespace, month], queryFn: ({ signal }) => api.storageUsage(namespace, month, signal), retry: false });
  const reconcile = useMutation({ mutationFn: () => api.reconcileStorage(namespace), onSuccess: () => qc.invalidateQueries({ queryKey: ['storage', namespace] }) });
  if (usage.isPending) return <p aria-live="polite">{t('storage.loading')}</p>;
  if (usage.isError) return <p role="alert">{t('storage.unavailable')} <button type="button" onClick={() => void usage.refetch()}>{t('storage.retry')}</button></p>;
  const data = usage.data;
  const labels = { metering: t('storage.metering'), active: t('storage.active'), warning: t('storage.warning'), overage: t('storage.overage'), grace: t('storage.grace'), read_only: t('storage.read_only') };
  return <section className="storage-usage" aria-label={t('storage.title')}>
    <h3>{t('storage.title')}{data.policy.plan && <> · {data.policy.plan === 'enterprise' ? 'Enterprise' : data.policy.plan === 'team' ? 'Team' : 'Free'}</>}</h3>
    <p className={`storage-state ${data.state}`} role="status">{labels[data.state]}</p>
    {data.state === 'read_only' && <p className="warn-red">{t('storage.limited')}</p>}
    <dl className="storage-totals">
      <div><dt>{t('storage.current')}</dt><dd>{bytes(data.current_bytes)}</dd></div>
      <div><dt>{t('storage.included')}</dt><dd>{data.policy.plan ? bytes(data.policy.included_bytes) : '—'}</dd></div>
      <div><dt>{t('storage.excess')}</dt><dd>{bytes(data.excess_bytes)}</dd></div>
    </dl>
    <p className="hint">{t('storage.definition')}</p>
    {data.metered_since && <p className="hint">{t('storage.since', { date: new Date(data.metered_since).toLocaleDateString() })}</p>}
    <label>{t('storage.month')} <input type="month" value={month} max={new Date().toISOString().slice(0, 7)} onChange={e => { if (e.target.value) setMonth(e.target.value); }} /></label>
    <p>{t('storage.hours')}: <strong>{(Number(data.overage_byte_hours) / 2 ** 30).toLocaleString(undefined, { maximumFractionDigits: 4 })}</strong></p>
    <p className="hint">{t('storage.measured')}</p>
    {canReconcile && <button type="button" disabled={reconcile.isPending} onClick={() => reconcile.mutate()}>{t(reconcile.isPending ? 'storage.reconciling' : 'storage.reconcile')}</button>}
    {reconcile.isError && <p role="alert" className="err">{reconcile.error.message}</p>}
    <details><summary>{t('storage.ledger')}</summary><ol className="storage-ledger">{data.entries.map(e => <li key={e.sequence}>
      <time dateTime={e.occurred_at}>{new Date(e.occurred_at).toLocaleString()}</time>
      <span>{t(e.reason.startsWith('reconcile') ? 'storage.correction' : e.reason === 'policy.changed' ? 'storage.policy' : 'storage.change')}</span>
      <strong>{e.delta_bytes > 0 ? '+' : ''}{bytes(e.delta_bytes)}</strong>
    </li>)}</ol></details>
  </section>;
}
export function PersonalStorageUsage() {
  const t = useT(); const [open, setOpen] = useState(false);
  return <details open={open} onToggle={e => setOpen(e.currentTarget.open)}><summary>{t('storage.title')}</summary>{open && <StorageUsage namespace="self" canReconcile />}</details>;
}
