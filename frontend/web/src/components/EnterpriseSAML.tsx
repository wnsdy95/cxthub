import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { SAMLInput, SAMLView } from '../federation';

export function EnterpriseSAML({ id, owner }: { id: string; owner: boolean }) {
 const t = useT(); const qc = useQueryClient(); const key = ['enterpriseSAML', id];
 const query = useQuery({ queryKey: key, queryFn: () => api.samlView(id) });
 const refresh = () => Promise.all([qc.invalidateQueries({ queryKey: key }), qc.invalidateQueries({ queryKey: ['enterpriseOIDC', id] }), qc.invalidateQueries({ queryKey: ['enterpriseAudit', id] })]);
 const verify = useMutation({ mutationFn: () => api.samlAuthorize(id), onSuccess: ({ url }) => window.location.assign(url) });
 const disable = useMutation({ mutationFn: (revision: string) => api.samlDisable(id, revision), onSettled: refresh });
 const view = query.data; const error = query.error || verify.error || disable.error;
 return <section className="management-policy enterprise-identity" aria-label={t('enterpriseSAML.title')}>
  <h2>{t('enterpriseSAML.title')}</h2><p className="hint">{t('enterpriseIdentity.note')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {query.isPending && <p className="hint">{t('common.loading')}</p>}
  {view && !view.available && <p className="hint">{t('enterpriseIdentity.unavailable')}</p>}
  {view?.available && <>
   {view.configured ? <>
    <p>{t('enterpriseIdentity.provider')} <code>{view.connection?.issuer}</code></p>
    <p className="hint">{view.verified_until ? <>{t('enterpriseIdentity.verified')} <time>{new Date(view.verified_until).toLocaleString()}</time></> : t('enterpriseIdentity.unverified')}</p>
    <button disabled={verify.isPending} onClick={() => verify.mutate()}>{t('enterpriseSAML.verify')}</button>
    <p><a href={view.entity_id} target="_blank" rel="noreferrer">{t('enterpriseSAML.download')}</a></p>
   </> : <p className="hint">{t('enterpriseIdentity.empty')}</p>}
   {owner && <>
    <SAMLEditor key={view.connection?.revision ?? 'new'} id={id} view={view} onSaved={refresh} />
    {view.connection && <button className="ghost" disabled={disable.isPending} onClick={() => disable.mutate(view.connection!.revision)}>{t('enterpriseIdentity.disable')}</button>}
   </>}
  </>}
 </section>;
}
function SAMLEditor({ id, view, onSaved }: { id: string; view: SAMLView; onSaved: () => Promise<unknown> }) {
 const t = useT();
 const [form, setForm] = useState<SAMLInput>({ domain: view.connection?.domain ?? '', metadata: '', revision: view.connection?.revision ?? '' });
 const save = useMutation({ mutationFn: () => api.samlConfigure(id, form), onSuccess: async () => { setForm(f => ({ ...f, metadata: '' })); await onSaved(); } });
 return <details className="identity-editor"><summary>{t('enterpriseSAML.configure')}</summary><form onSubmit={e => { e.preventDefault(); save.mutate(); }}>
  <p className="hint">{t('enterpriseSAML.setup')}</p>
  <label>{t('enterpriseSAML.entity')}<input readOnly value={view.entity_id} /></label>
  <label>{t('enterpriseSAML.acs')}<input readOnly value={view.acs} /></label>
  <label>{t('enterpriseIdentity.domain')}<input required maxLength={253} value={form.domain} disabled={save.isPending} onChange={e => setForm({ ...form, domain: e.target.value })} /></label>
  <label>{t('enterpriseSAML.metadata')}<textarea required rows={8} maxLength={262144} value={form.metadata} disabled={save.isPending} onChange={e => setForm({ ...form, metadata: e.target.value })} /></label>
  <button disabled={save.isPending}>{t('enterpriseIdentity.save')}</button>
  {save.error && <p className="err" role="alert">{save.error.message}</p>}
 </form></details>;
}
