import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { EnterpriseDomain } from '../enterpriseDomains';

export function EnterpriseDomains({ id, owner }: { id: string; owner: boolean }) {
 const t = useT(); const qc = useQueryClient(); const [name, setName] = useState('');
 const key = ['enterpriseDomains', id];
 const list = useQuery({ queryKey: key, queryFn: () => api.enterpriseDomains(id) });
 const mutation = useMutation({
  mutationFn: ({ action, domain, revision }: { action: 'request' | 'verify' | 'release'; domain: string; revision: string }) => api.enterpriseDomainCommand(id, action, domain, revision),
  onSuccess: () => setName(''),
  onSettled: async () => { await Promise.all([qc.invalidateQueries({ queryKey: key }), qc.invalidateQueries({ queryKey: ['enterpriseAudit', id] })]); },
 });
 const run = (action: 'request' | 'verify' | 'release', row: EnterpriseDomain) => mutation.mutate({ action, domain: row.domain, revision: row.revision });
 const error = mutation.error || list.error;
 return <section className="management-policy enterprise-domains" aria-label={t('enterpriseDomains.title')}>
  <h2>{t('enterpriseDomains.title')}</h2><p className="hint">{t('enterpriseDomains.note')}</p>
  {owner && <form className="management-form" onSubmit={(event) => { event.preventDefault(); mutation.mutate({ action: 'request', domain: name, revision: '' }); }}>
   <label>{t('enterpriseDomains.name')}<input value={name} onChange={(event) => setName(event.target.value)} required maxLength={253} placeholder="example.com" disabled={mutation.isPending} /></label>
   <button disabled={mutation.isPending || !name.trim() || Boolean(list.error)}>{t('enterpriseDomains.request')}</button>
  </form>}
  {error && <p className="err" role="alert">{error.message}</p>}
  {list.isPending && <p className="hint">{t('common.loading')}</p>}
  {list.data?.length === 0 && <p className="hint">{t('enterpriseDomains.empty')}</p>}
  {list.data?.map((row) => <article className="enterprise-domain-card" key={row.domain}>
   <h3>{row.domain} <span className="role">{t(`enterpriseDomains.${row.state}`)}</span></h3>
   <label>{t('enterpriseDomains.recordName')}<input readOnly value={row.record_name} /></label>
   <label>{t('enterpriseDomains.recordValue')}<input readOnly value={row.challenge} /></label>
   <p className="hint">{t('enterpriseDomains.challengeExpiry')} <time>{new Date(row.challenge_expires_at).toLocaleString()}</time></p>
   {row.state === 'verified' && <p className="hint">{t('enterpriseDomains.verifiedUntil')} <time>{new Date(row.verified_until).toLocaleString()}</time></p>}
   {owner && <div className="management-form">
    <button disabled={mutation.isPending} onClick={() => run('verify', row)}>{t('enterpriseDomains.verify')}</button>
    <button className="ghost" disabled={mutation.isPending} onClick={() => run('request', row)}>{t('enterpriseDomains.renew')}</button>
    <button className="ghost" disabled={mutation.isPending} onClick={() => run('release', row)}>{t('enterpriseDomains.release')}</button>
   </div>}
  </article>)}
 </section>;
}
