import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { OIDCInput, OIDCView } from '../federation';
import { EnterpriseSAML } from './EnterpriseSAML';
import { EnterpriseCredentialAssurances } from './EnterpriseCredentialAssurances';

export function EnterpriseIdentity({ id, owner }: { id: string; owner: boolean }) {
 return <><OIDCIdentity id={id} owner={owner} /><EnterpriseSAML id={id} owner={owner} /><EnterpriseCredentialAssurances id={id} owner={owner} /></>;
}
function OIDCIdentity({ id, owner }: { id: string; owner: boolean }) {
 const t = useT(); const qc = useQueryClient();
 const key = ['enterpriseOIDC', id];
 const query = useQuery({ queryKey: key, queryFn: () => api.oidcView(id) });
 const refresh = () => Promise.all([qc.invalidateQueries({ queryKey: key }), qc.invalidateQueries({ queryKey: ['enterpriseAudit', id] })]);
 const verify = useMutation({ mutationFn: () => api.oidcAuthorize(id), onSuccess: ({ url }) => { window.location.assign(url); } });
 const disable = useMutation({ mutationFn: (revision: string) => api.oidcDisable(id, revision), onSettled: refresh });
 const error = query.error || verify.error || disable.error;
 const view = query.data;
 return <section className="management-policy enterprise-identity" aria-label={t('enterpriseIdentity.title')}>
  <h2>{t('enterpriseIdentity.title')}</h2><p className="hint">{t('enterpriseIdentity.note')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {query.isPending && <p className="hint">{t('common.loading')}</p>}
  {view && !view.available && <p className="hint">{t('enterpriseIdentity.unavailable')}</p>}
  {view?.available && <>
   {view.configured ? <>
    <p>{t('enterpriseIdentity.provider')} <code>{view.connection?.issuer}</code></p>
    <p className="hint">{view.verified_until ? <>{t('enterpriseIdentity.verified')} <time>{new Date(view.verified_until).toLocaleString()}</time></> : t('enterpriseIdentity.unverified')}</p>
    <button disabled={verify.isPending} onClick={() => verify.mutate()}>{t('enterpriseIdentity.verify')}</button>
   </> : <p className="hint">{t('enterpriseIdentity.empty')}</p>}
   {owner && <>
    <OIDCEditor key={view.connection?.revision ?? 'new'} id={id} view={view} onSaved={refresh} />
    {view.connection && <button className="ghost" disabled={disable.isPending} onClick={() => disable.mutate(view.connection!.revision)}>{t('enterpriseIdentity.disable')}</button>}
   </>}
  </>}
 </section>;
}

function OIDCEditor({ id, view, onSaved }: { id: string; view: OIDCView; onSaved: () => Promise<unknown> }) {
 const t = useT();
 const [form, setForm] = useState<OIDCInput>({ domain: view.connection?.domain ?? '', issuer: view.connection?.issuer ?? '', client_id: view.connection?.client_id ?? '', client_secret: '', auth_method: view.connection?.auth_method ?? 'client_secret_basic', revision: view.connection?.revision ?? '' });
 const mutation = useMutation({ mutationFn: () => api.oidcConfigure(id, form), onSuccess: async () => { setForm((f) => ({ ...f, client_secret: '' })); await onSaved(); } });
 return <details className="identity-editor"><summary>{t('enterpriseIdentity.configure')}</summary><form onSubmit={(e) => { e.preventDefault(); mutation.mutate(); }}>
  <p className="hint">{t('enterpriseIdentity.setupNote')}</p>
  <label>{t('enterpriseIdentity.callback')}<input readOnly value={view.callback_uri} /></label>
  {(['domain', 'issuer', 'client_id', 'client_secret'] as const).map((field) => <label key={field}>{t(`enterpriseIdentity.${field}`)}<input type={field === 'client_secret' ? 'password' : 'text'} autoComplete={field === 'client_secret' ? 'new-password' : 'off'} value={form[field]} onChange={(e) => setForm({ ...form, [field]: e.target.value })} required maxLength={field === 'client_secret' ? 4096 : 512} disabled={mutation.isPending} /></label>)}
  <label>{t('enterpriseIdentity.method')}<select value={form.auth_method} onChange={(e) => setForm({ ...form, auth_method: e.target.value as OIDCInput['auth_method'] })}><option value="client_secret_basic">client_secret_basic</option><option value="client_secret_post">client_secret_post</option></select></label>
  <button disabled={mutation.isPending}>{t('enterpriseIdentity.save')}</button>
  {mutation.error && <p className="err" role="alert">{mutation.error.message}</p>}
 </form></details>;
}
