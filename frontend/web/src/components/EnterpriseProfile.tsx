import { RenameSpace } from './NamespaceAdministration';
import { InvitationManager } from './CollaborationInvitations';
import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useMe, useOrganizations } from '../hooks';
import { useT } from '../i18n';
import { enterprisePath, navigate } from '../route';
import { safeAvatarUrl } from '../urls';
import type { Enterprise, EnterpriseMembership, EnterprisePolicy } from '../types';
import { Logo } from './Logo';
import { Avatar, avatarColor } from './Avatar';
import { resizeToDataURL } from './Settings';

function useEnterprises() { return useQuery({ queryKey: ['enterprises'], queryFn: api.listEnterprises, enabled: Boolean(useMe().data) }); }

export function MyEnterprises() {
 const t = useT(); const qc = useQueryClient(); const list = useEnterprises();
 const [name, setName] = useState(''); const [slug, setSlug] = useState('');
 const create = useMutation({ mutationFn: () => api.createEnterprise(name, slug), onSuccess: async (created) => { await qc.invalidateQueries({ queryKey: ['enterprises'] }); navigate(enterprisePath(created.slug)); } });
 return <section className="profile-organizations"><div className="profile-section-head"><h2 className="profile-section">{t('enterprise.title')}</h2><span className="count-badge">{list.data?.length ?? 0}</span></div>
  <div className="organization-list">{list.data?.map((enterprise) => <button className="organization-card" key={enterprise.id} onClick={() => navigate(enterprisePath(enterprise.slug))}><strong>{enterprise.name}</strong><small>/enterprises/{enterprise.slug}</small></button>)}</div>
  {list.data?.length === 0 && <p className="hint">{t('enterprise.empty')}</p>}
  <details className="organization-create"><summary>{t('enterprise.create')}</summary><form className="management-form" onSubmit={(event) => { event.preventDefault(); create.mutate(); }}>
   <label>{t('enterprise.name')}<input value={name} onChange={(event) => setName(event.target.value)} required maxLength={128} /></label><label>{t('enterprise.slug')}<input value={slug} onChange={(event) => setSlug(event.target.value)} maxLength={64} /></label><button disabled={create.isPending || !name.trim()}>{t('enterprise.create')}</button>
  </form></details>{(list.error || create.error) && <p className="err" role="alert">{(list.error ?? create.error)?.message}</p>}
 </section>;
}

export function EnterpriseProfile({ slug }: { slug: string }) {
 const t = useT(); const me = useMe().data;
 const query = useQuery({ queryKey: ['enterprise', slug], queryFn: () => api.getEnterprise(slug), retry: false });
 return <div className="app"><header className="topbar"><button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}><div className="brand sm"><Logo /></div></button><div className="who">{me && <Avatar user={me} link />}</div></header>
  {query.isLoading ? <div className="loading">…</div> : query.data ? <EnterpriseBody key={query.data.id} enterprise={query.data} /> : <div className="empty-box" role="alert">{t('enterprise.unavailable')}</div>}
 </div>;
}

function EnterpriseBody({ enterprise }: { enterprise: Enterprise }) {
 const t = useT(); const qc = useQueryClient(); const me = useMe().data;
 const members = useQuery({ queryKey: ['enterpriseMembers', enterprise.id], queryFn: () => api.listEnterpriseMembers(enterprise.id) });
 const organizations = useQuery({ queryKey: ['enterpriseOrganizations', enterprise.id], queryFn: () => api.listEnterpriseOrganizations(enterprise.id) });
 const mine = useOrganizations();
 const role = members.data?.find((member) => member.user_id === me?.id)?.role;
 const canAdmin = role === 'owner' || role === 'admin';
 const [tab, setTab] = useState<'organizations' | 'members' | 'policies' | 'audit' | 'settings'>('organizations');
 const [organizationId, setOrganizationId] = useState('');
 const audit = useQuery({ queryKey: ['enterpriseAudit', enterprise.id], queryFn: () => api.listEnterpriseAudit(enterprise.id), enabled: canAdmin && tab === 'audit' });
 const mutation = useMutation({ mutationFn: (run: () => Promise<unknown>) => run(), onSuccess: async () => { await Promise.all([
  qc.invalidateQueries({ queryKey: ['enterprise', enterprise.slug] }), qc.invalidateQueries({ queryKey: ['enterprises'] }),
  qc.invalidateQueries({ queryKey: ['enterpriseMembers', enterprise.id] }), qc.invalidateQueries({ queryKey: ['enterpriseOrganizations', enterprise.id] }),
  qc.invalidateQueries({ queryKey: ['enterpriseAudit', enterprise.id] }), qc.invalidateQueries({ queryKey: ['organizationEffectivePolicy'] }),
 ]); } });
 const busy = mutation.isPending;
 const error = members.error ?? organizations.error ?? mine.error ?? mutation.error ?? audit.error;
 const logo = safeAvatarUrl(enterprise.logo);
 const memberLabel = (member: EnterpriseMembership) => member.user?.nickname || member.user?.name || member.user?.username || member.user_id;
 return <div className="profile"><div className="profile-grid">
  <aside className="profile-side">{logo ? <img className="avatar-lg avatar-img organization-logo" src={logo} alt={enterprise.name} /> : <div className="avatar-lg organization-logo" style={{ background: avatarColor(enterprise.slug) }} aria-hidden="true">{enterprise.name.charAt(0)}</div>}<h1 className="profile-name">{enterprise.name}</h1><p className="profile-handle">{enterprise.slug}</p><span className="organization-badge">{t('enterprise.title')}</span></aside>
  <main className="profile-main"><p className="organization-access-note">{t('enterprise.note')}</p>
   <nav className="tabs organization-tabs" aria-label={t('enterprise.title')}>{(['organizations', 'members', 'policies', 'audit', 'settings'] as const).filter((item) => canAdmin || (item !== 'audit' && item !== 'settings')).map((item) => <button key={item} className={`tab${tab === item ? ' on' : ''}`} onClick={() => setTab(item)}>{t(`enterprise.${item}`)}</button>)}</nav>
   {error && <p className="err" role="alert">{error.message}</p>}
   {tab === 'organizations' && <section>
    {role === 'owner' && <form className="management-form" onSubmit={(event) => { event.preventDefault(); mutation.mutate(() => api.linkEnterpriseOrganization(enterprise.id, organizationId)); }}>
     <select aria-label={t('enterprise.chooseOrganization')} value={organizationId} onChange={(event) => setOrganizationId(event.target.value)} required><option value="">{t('enterprise.chooseOrganization')}</option>{mine.data?.filter((organization) => organization.effective_role === 'owner' && !organizations.data?.some((item) => item.id === organization.id)).map((organization) => <option key={organization.id} value={organization.id}>{organization.name}</option>)}</select><button disabled={busy || !organizationId}>{t('enterprise.link')}</button>
    </form>}
    <ul className="management-rows">{organizations.data?.map((organization) => <li key={organization.id}><span><button className="linkish" onClick={() => navigate(`/${organization.slug}`)}>{organization.name}</button></span>{role === 'owner' && <button className="ghost mini" disabled={busy} onClick={() => mutation.mutate(() => api.unlinkEnterpriseOrganization(enterprise.id, organization.id))}>{t('enterprise.unlink')}</button>}</li>)}</ul>
   </section>}
   {tab === 'members' && <section>
    {canAdmin && <InvitationManager kind="enterprise" spaceId={enterprise.id} owner={role === 'owner'} />}
    <ul className="management-rows">{members.data?.map((member) => <li key={member.user_id}><span>{memberLabel(member)}</span>{role === 'owner' || role === 'admin' && member.role === 'member' ? <><select aria-label={`${memberLabel(member)} ${t('dashboard.roleChangeAria')}`} value={member.role} disabled={busy} onChange={(event) => { const value = event.target.value as EnterpriseMembership['role']; mutation.mutate(() => api.setEnterpriseMember(enterprise.id, member.user_id, value)); }}>{(role === 'owner' ? ['member', 'admin', 'owner'] : ['member']).map((item) => <option key={item} value={item}>{item}</option>)}</select><button className="ghost mini" disabled={busy} onClick={() => mutation.mutate(() => api.removeEnterpriseMember(enterprise.id, member.user_id))}>{t('common.remove')}</button></> : <span className="role">{member.role}</span>}</li>)}</ul>
   </section>}
   {tab === 'policies' && <EnterprisePolicies key={JSON.stringify(enterprise.policy)} value={enterprise.policy} disabled={!canAdmin || busy} onSave={(policy) => mutation.mutate(() => api.updateEnterprise(enterprise.id, { policy }))} />}
   {tab === 'audit' && canAdmin && <ul className="management-rows">{audit.data?.map((event) => <li key={event.id}><span>{event.action}<small> · {event.target_id}</small></span><time>{new Date(event.created_at).toLocaleString()}</time></li>)}</ul>}
   {tab === 'settings' && role === 'owner' && <RenameSpace key={enterprise.slug} kind="enterprise" id={enterprise.id} slug={enterprise.slug} />}
   {tab === 'settings' && canAdmin && <EnterpriseSettings enterprise={enterprise} disabled={busy} onSave={(patch) => mutation.mutate(() => api.updateEnterprise(enterprise.id, patch))} />}
  </main>
 </div></div>;
}
function EnterprisePolicies({ value, disabled, onSave }: { value: EnterprisePolicy; disabled: boolean; onSave: (value: EnterprisePolicy) => void }) {
 const t = useT(); const [policy, setPolicy] = useState(value);
 return <form className="management-policy" onSubmit={(event) => { event.preventDefault(); onSave(policy); }}><p className="hint">{t('enterprise.policyHint')}</p>
  <label>{t('enterprise.creation')}<select value={policy.repository_creation} disabled={disabled} onChange={(event) => setPolicy({ ...policy, repository_creation: event.target.value as EnterprisePolicy['repository_creation'] })}><option value="admins">{t('enterprise.adminsOnly')}</option><option value="members">{t('enterprise.membersAllowed')}</option></select></label>
  <label><input type="checkbox" checked={policy.allow_public_repositories} disabled={disabled} onChange={(event) => setPolicy({ ...policy, allow_public_repositories: event.target.checked })} />{t('enterprise.public')}</label>
  <label><input type="checkbox" checked={policy.allow_break_glass} disabled={disabled} onChange={(event) => setPolicy({ ...policy, allow_break_glass: event.target.checked })} />{t('enterprise.breakGlass')}</label>
  {!disabled && <button>{t('common.save')}</button>}
 </form>;
}
function EnterpriseSettings({ enterprise, disabled, onSave }: { enterprise: Enterprise; disabled: boolean; onSave: (patch: { name: string; logo: string }) => void }) {
 const t = useT(); const [name, setName] = useState(enterprise.name); const [logo, setLogo] = useState(enterprise.logo ?? ''); const [error, setError] = useState('');
 const submit = (event: FormEvent) => { event.preventDefault(); onSave({ name, logo }); };
 return <form className="management-policy" onSubmit={submit}><label>{t('enterprise.name')}<input value={name} onChange={(event) => setName(event.target.value)} required maxLength={128} disabled={disabled} /></label><label>{t('organization.uploadLogo')}<input type="file" accept="image/png,image/jpeg,image/webp" disabled={disabled} onChange={async (event) => { const file = event.target.files?.[0]; if (!file) return; try { setLogo(await resizeToDataURL(file, t)); setError(''); } catch (error) { setError(String(error)); } }} /></label>{safeAvatarUrl(logo) && <img className="avatar-lg avatar-img organization-logo" src={logo} alt={name} />}<button disabled={disabled || !name.trim()}>{t('common.save')}</button>{error && <p className="err" role="alert">{error}</p>}</form>;
}
