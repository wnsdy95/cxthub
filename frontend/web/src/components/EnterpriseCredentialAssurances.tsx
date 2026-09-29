import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { AssurancePolicy, CredentialApprovalInput } from '../federation';

export function EnterpriseCredentialAssurances({ id, owner }: { id: string; owner: boolean }) {
 const t = useT(); const qc = useQueryClient(); const key = ['credentialAssurances', id];
 const query = useQuery({ queryKey: key, queryFn: () => api.credentialAssurances(id) });
 const refresh = () => Promise.all([
  qc.invalidateQueries({ queryKey: key }), qc.invalidateQueries({ queryKey: ['enterpriseAudit', id] }),
  qc.invalidateQueries({ queryKey: ['enterpriseOIDC', id] }), qc.invalidateQueries({ queryKey: ['enterpriseSAML', id] }),
 ]);
 const approve = useMutation({ mutationFn: ({ target, proof }: { target: string; proof: CredentialApprovalInput }) => api.approveCredential(id, target, proof), onSettled: refresh });
 const revoke = useMutation({ mutationFn: (target: string) => api.revokeCredentialAssurance(id, target), onSettled: refresh });
 const data = query.data; const error = query.error || approve.error || revoke.error;
 // Refresh at the next displayed deadline, not on a polling loop. Server
 // commands still recheck every condition; the timer only keeps labels current.
 useEffect(() => {
  const deadlines = [data?.approval_available_until, ...(data?.credentials ?? []).flatMap(c => [c.expires_at, c.state === 'approved' ? c.verified_until : undefined])]
   .filter((d): d is string => Boolean(d)).map(d => new Date(d).getTime()).filter(d => d > Date.now());
  if (!deadlines.length) return;
  const timer = window.setTimeout(() => { void qc.invalidateQueries({ queryKey: ['credentialAssurances', id] }); }, Math.min(2_147_483_647, Math.max(1, Math.min(...deadlines) - Date.now() + 100)));
  return () => window.clearTimeout(timer);
 }, [data, qc, id]);
 return <section className="management-policy enterprise-identity" aria-label={t('credentialAssurance.title')}>
  <h2>{t('credentialAssurance.title')}</h2><p className="hint">{t('credentialAssurance.note')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {query.isPending && <p>{t('common.loading')}</p>}
  {data && !data.available && <p className="hint">{t('enterpriseIdentity.unavailable')}</p>}
  {data?.available && <>
   <p className="hint">{data.browser_proof ? t('credentialAssurance.ready') : t('credentialAssurance.verifyFirst')}</p>
   <button className="ghost" disabled={query.isFetching} onClick={() => { void refresh(); }}>{t('credentialAssurance.refresh')}</button>
   {data.credentials.length === 0 && <p className="hint">{t('credentialAssurance.empty')}</p>}
   <ul className="credential-assurances">{data.credentials.map(c => <li key={c.id}>
    <div><strong>{c.kind.toUpperCase()} · {c.label || t('credentialAssurance.unnamed')}</strong> <code>{c.id}</code>
     {c.hint && <small>…{c.hint}</small>}
     <p>{t(`credentialAssurance.${c.state}`)}{c.verified_until && <> · {t('credentialAssurance.until')} <time>{new Date(c.verified_until).toLocaleString()}</time></>}</p>
     <p className="hint">{t('credentialAssurance.created')} <time>{new Date(c.created_at).toLocaleString()}</time> · {t('credentialAssurance.expires')} <time>{new Date(c.expires_at).toLocaleString()}</time></p>
    </div>
    <div className="credential-actions">
     <button disabled={!data.browser_proof || approve.isPending || revoke.isPending} onClick={() => { if (data.browser_proof) approve.mutate({ target: c.id, proof: data.browser_proof }); }}>{t('credentialAssurance.approve')}</button>
     {c.state !== 'unapproved' && c.state !== 'revoked' && <button className="ghost" disabled={approve.isPending || revoke.isPending} onClick={() => revoke.mutate(c.id)}>{t('credentialAssurance.revoke')}</button>}
    </div>
   </li>)}</ul>
   {data.policy && (owner ? <AssurancePolicyEditor key={data.policy.revision} id={id} policy={data.policy} onSaved={refresh} /> : <p>{t('credentialAssurance.duration')}: {data.policy.max_age_hours}h</p>)}
  </>}
 </section>;
}

function AssurancePolicyEditor({ id, policy, onSaved }: { id: string; policy: AssurancePolicy; onSaved: () => Promise<unknown> }) {
 const t = useT(); const [hours, setHours] = useState(policy.max_age_hours);
 const save = useMutation({ mutationFn: () => api.assurancePolicy(id, { revision: policy.revision, max_age_hours: hours }), onSettled: onSaved });
 return <details className="identity-editor"><summary>{t('credentialAssurance.duration')}</summary>
  <form onSubmit={e => { e.preventDefault(); save.mutate(); }}>
   <p className="hint">{t('credentialAssurance.policyNote')}</p>
   <label>{t('credentialAssurance.hours')}<input type="number" min={1} max={24} step={1} value={hours} required onChange={e => setHours(Number(e.target.value))} disabled={save.isPending} /></label>
   <button disabled={save.isPending || hours === policy.max_age_hours}>{t('credentialAssurance.save')}</button>
   {save.error && <p className="err" role="alert">{save.error.message}</p>}
  </form>
 </details>;
}
