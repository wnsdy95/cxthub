import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { PreparedOwnerRecovery } from '../federation';

export function EnterpriseOwnerRecovery({ id }: { id: string }) {
 const t = useT(); const qc = useQueryClient();
 const key = ['ownerRecovery', id];
 const query = useQuery({ queryKey: key, queryFn: () => api.ownerRecovery(id) });
 const assessment = useQuery({ queryKey: ['credentialAssessment', id], queryFn: () => api.credentialAssessment(id) });
 const [draft, setDraft] = useState<PreparedOwnerRecovery>();
 const [confirmation, setConfirmation] = useState(''); const [code, setCode] = useState('');
 const refresh = () => Promise.all([qc.invalidateQueries({ queryKey: key }), qc.invalidateQueries({ queryKey: ['credentialAssessment', id] }), qc.invalidateQueries({ queryKey: ['enterpriseAudit', id] })]);
 // Secrets stay only in mounted component state; never in query/mutation data,
 // persistent storage, URLs or error text. Commands recheck all displayed state.
 const prepare = useMutation({ gcTime: 0, mutationFn: async () => { const p = await api.prepareOwnerRecovery(id, query.data!.revision); setDraft(p); setConfirmation(''); }, onSettled: refresh });
 const confirm = useMutation({ gcTime: 0, mutationFn: async () => { if (draft) await api.confirmOwnerRecovery(id, draft.revision, confirmation); }, onSuccess: () => { setDraft(undefined); setConfirmation(''); }, onSettled: refresh });
 const revoke = useMutation({ gcTime: 0, mutationFn: () => api.revokeOwnerRecovery(id, query.data!.revision), onSuccess: () => { setDraft(undefined); setConfirmation(''); }, onSettled: refresh });
 const redeem = useMutation({ gcTime: 0, mutationFn: async () => { const value = code; setCode(''); await api.redeemOwnerRecovery(id, value); }, onSettled: refresh });
 const data = query.data; const proof = assessment.data;
 const until = proof?.state === 'verified' && proof.authenticated_at && proof.verified_until ? Math.min(new Date(proof.authenticated_at).getTime() + 600_000, new Date(proof.verified_until).getTime()) : 0;
 const fresh = until > Date.now(); const busy = prepare.isPending || confirm.isPending || revoke.isPending || redeem.isPending;
 useEffect(() => {
  const deadlines = [until, data?.repair_until ? new Date(data.repair_until).getTime() : 0, draft ? new Date(draft.pending_until).getTime() : 0].filter(x => x > Date.now());
  if (!deadlines.length) return;
  const timer = window.setTimeout(() => {
   if (draft && new Date(draft.pending_until).getTime() <= Date.now()) { setDraft(undefined); setConfirmation(''); }
   void qc.invalidateQueries({ queryKey: ['ownerRecovery', id] }); void qc.invalidateQueries({ queryKey: ['credentialAssessment', id] });
  }, Math.max(1, Math.min(...deadlines) - Date.now() + 100));
  return () => window.clearTimeout(timer);
 }, [until, data?.repair_until, draft, qc, id]);
 const error = query.error || prepare.error || confirm.error || revoke.error || redeem.error;
 return <section className="management-policy enterprise-identity owner-recovery" aria-label={t('ownerRecovery.title')}>
  <h2>{t('ownerRecovery.title')}</h2><p className="hint">{t('ownerRecovery.note')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {query.isPending && <p>{t('common.loading')}</p>}
  {data && !data.available && <p>{t('enterpriseIdentity.unavailable')}</p>}
  {data?.available && <>
   <p role="status">{t(`ownerRecovery.${data.state}`)}</p>
   {data.repair_until && new Date(data.repair_until).getTime() > Date.now() && <p>{t('ownerRecovery.repairUntil')} <time>{new Date(data.repair_until).toLocaleString()}</time></p>}
   {!fresh && <p className="hint">{t('ownerRecovery.verifyFirst')}</p>}
   <button className="ghost" disabled={query.isFetching} onClick={() => { void refresh(); }}>{t('ownerRecovery.refresh')}</button>
   <button disabled={!fresh || busy} onClick={() => prepare.mutate()}>{t(data.ready ? 'ownerRecovery.replace' : 'ownerRecovery.prepare')}</button>
   {draft && <div className="recovery-draft">
    <p>{t('ownerRecovery.saveNote')}</p><code className="recovery-code">{draft.code}</code>
    <form onSubmit={e => { e.preventDefault(); confirm.mutate(); }}>
     <label>{t('ownerRecovery.confirmCode')}<input type="password" autoComplete="off" value={confirmation} onChange={e => setConfirmation(e.target.value)} maxLength={128} required disabled={busy} /></label>
     <button disabled={busy || !fresh || !confirmation}>{t('ownerRecovery.confirm')}</button>
    </form>
   </div>}
   {data.ready && <button className="ghost" disabled={!fresh || busy} onClick={() => revoke.mutate()}>{t('ownerRecovery.revoke')}</button>}
   <details><summary>{t('ownerRecovery.use')}</summary><p className="hint">{t('ownerRecovery.useNote')}</p>
    <form onSubmit={e => { e.preventDefault(); redeem.mutate(); }}>
     <label>{t('ownerRecovery.savedCode')}<input type="password" autoComplete="off" value={code} onChange={e => setCode(e.target.value)} maxLength={128} required disabled={busy} /></label>
     <button disabled={busy || !code}>{t('ownerRecovery.redeem')}</button>
    </form>
   </details>
  </>}
 </section>;
}
